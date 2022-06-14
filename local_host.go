package contest

import (
	"bytes"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"

	dockerClient "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/pkg/errors"
	"github.com/spikeekips/mitum/util"
)

var DefaultLocalHostURI = &url.URL{Host: "localhost"}

type LocalHost struct {
	*baseHost
}

func NewLocalHost() (*LocalHost, error) {
	client, err := dockerClient.NewClientWithOpts(
		dockerClient.FromEnv,
	)
	if err != nil {
		return nil, errors.Wrap(err, "")
	}

	h := &LocalHost{baseHost: newBaseHost(DefaultLocalHostURI, client)}

	if err := h.checkArch(); err != nil {
		return nil, errors.Wrap(err, "")
	}

	return h, nil
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
	e := util.StringErrorFunc("failed to sftp")

	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return errors.Wrapf(err, "failed to create upload dest file for %q", dest)
	}

	defer func() {
		_ = f.Close()
	}()

	if _, err := io.Copy(f, s); err != nil {
		return e(err, "")
	}

	return nil
}
