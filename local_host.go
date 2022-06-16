package contest

import (
	"bytes"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	dockerClient "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/pkg/errors"
	"github.com/spikeekips/mitum/util"
)

var localHostURI = &url.URL{Host: "localhost"}

type LocalHost struct {
	*baseHost
}

func NewLocalHost(base string, dockerhost *url.URL) (*LocalHost, error) {
	var client *dockerClient.Client

	switch {
	case dockerhost != nil:
		i, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(dockerhost.String()))
		if err != nil {
			return nil, errors.Wrap(err, "")
		}

		client = i
	default:
		i, err := dockerClient.NewClientWithOpts(dockerClient.FromEnv)
		if err != nil {
			return nil, errors.Wrap(err, "")
		}

		client = i
	}

	bh, err := newBaseHost(base, localHostURI, client)
	if err != nil {
		return nil, errors.Wrap(err, "")
	}

	h := &LocalHost{baseHost: bh}

	switch u, err := user.Current(); {
	case err != nil:
		return nil, errors.Wrap(err, "")
	default:
		h.user = u.Uid
	}

	if err := h.checkArch(); err != nil {
		return nil, errors.Wrap(err, "")
	}

	if err := os.MkdirAll(h.base, 0o700); err != nil {
		return nil, errors.Wrap(err, "failed to create base")
	}

	return h, nil
}

func (h *LocalHost) ContainerFreePort(id, network, innerPort string) (string, error) {
	return h.containerFreePort(id, network, innerPort, func(portmap nat.PortMap) (string, error) {
		return h.baseHost.freePort(network, portmap, func() (string, error) {
			return AvailablePort(network)
		})
	})
}

func (h *LocalHost) FreePort(network string) (string, error) {
	return h.baseHost.freePort(network, nat.PortMap{}, func() (string, error) {
		return AvailablePort(network)
	})
}

func (h *LocalHost) Upload(s io.Reader, dest string, mode os.FileMode) error {
	e := util.StringErrorFunc("failed to upload")

	newdest := filepath.Join(h.base, dest)

	f, err := os.OpenFile(newdest, os.O_WRONLY|os.O_CREATE, mode)
	if err != nil {
		return e(err, "failed to create upload dest file for %q", newdest)
	}

	defer func() {
		_ = f.Close()
	}()

	if _, err := io.Copy(f, s); err != nil {
		return e(err, "")
	}

	return nil
}

func (h *LocalHost) Mkdir(dest string, mode os.FileMode) error {
	newdest := filepath.Join(h.base, dest)

	if err := os.MkdirAll(filepath.Clean(newdest), mode); err != nil {
		return errors.Wrap(err, "")
	}

	return nil
}

func (h *LocalHost) checkArch() error {
	e := util.StringErrorFunc("failed to check arch")

	var b bytes.Buffer

	cmd := exec.Command("uname", "-sm")
	cmd.Stdout = &b

	if err := cmd.Run(); err != nil {
		return e(err, "")
	}

	uname := strings.TrimSuffix(b.String(), "\n")

	arch, found := supportedArchs[uname]
	if !found {
		return e(nil, "not supported arch, %q", uname)
	}

	h.arch = arch

	return nil
}
