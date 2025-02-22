package contest

import (
	"archive/tar"
	"bytes"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	dockerClient "github.com/docker/docker/client"
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
			return nil, errors.WithStack(err)
		}

		client = i
	default:
		i, err := dockerClient.NewClientWithOpts(dockerClient.FromEnv)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		client = i
	}

	bh, err := newBaseHost(base, localHostURI, client)
	if err != nil {
		return nil, err
	}

	h := &LocalHost{baseHost: bh}

	switch u, err := user.Current(); {
	case err != nil:
		return nil, errors.WithStack(err)
	default:
		h.user = u.Uid
	}

	if err := h.checkArch(); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(h.base, 0o700); err != nil {
		return nil, errors.Wrap(err, "create base")
	}

	return h, nil
}

func (h *LocalHost) FreePort(id, network string) (string, error) {
	return h.baseHost.freePort(id, network, AvailablePort)
}

func (h *LocalHost) Upload(s io.Reader, name, dest string, mode os.FileMode) error {
	e := util.StringError("upload")

	newdest := filepath.Join(h.base, dest)

	n, err := os.OpenFile(newdest, os.O_WRONLY|os.O_CREATE, mode)
	if err != nil {
		return e.WithMessage(err, "create upload dest file for %q", newdest)
	}

	defer func() {
		_ = n.Close()
	}()

	if _, err := io.Copy(n, s); err != nil {
		return e.Wrap(err)
	}

	h.addFile(name, newdest)

	return nil
}

func (h *LocalHost) CollectResult(outputfile string) error {
	e := util.StringError("collect result")

	o := outputfile

	// NOTE golang's gzipwriter too slow
	ext := filepath.Ext(o)
	if ext == ".gz" {
		o = o[:len(o)-len(ext)]
	}

	out, err := os.Create(o)
	if err != nil {
		return e.Wrap(err)
	}

	defer func() {
		_ = out.Close()
	}()

	tw := tar.NewWriter(out)
	defer func() {
		_ = tw.Close()
	}()

	addfile := func(filename string, info fs.FileInfo) error {
		f, err := os.Open(filename)
		if err != nil {
			return errors.WithStack(err)
		}

		defer func() {
			_ = f.Close()
		}()

		header, err := tar.FileInfoHeader(info, info.Name())
		if err != nil {
			return errors.WithStack(err)
		}

		header.Name, _ = filepath.Rel(h.base, filename)

		if err = tw.WriteHeader(header); err != nil {
			return errors.WithStack(err)
		}

		if _, err = io.Copy(tw, f); err != nil {
			return errors.WithStack(err)
		}

		return nil
	}

	if err := filepath.Walk(h.base, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		return addfile(path, info)
	}); err != nil {
		return e.Wrap(err)
	}

	return nil
}

func (h *LocalHost) Mkdir(dest string, mode os.FileMode) error {
	newdest := filepath.Join(h.base, dest)

	if err := os.MkdirAll(filepath.Clean(newdest), mode); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func (h *LocalHost) RunCommand(cmd string) (stdout, stderr string, ok bool, err error) {
	var e *exec.ExitError

	switch stdout, stderr, err = h.runCommand(cmd); {
	case err == nil:
		return stdout, stderr, true, nil
	case errors.As(err, &e):
		return stdout, stderr, false, nil
	default:
		return stdout, stderr, false, err
	}
}

func (h *LocalHost) checkArch() error {
	out, _, err := h.runCommand("uname -sm")
	if err != nil {
		return errors.WithMessage(err, "check arch")
	}

	uname := strings.TrimSuffix(out, "\n")

	arch, found := supportedArchs[uname]
	if !found {
		return errors.Errorf("not supported arch, %q", uname)
	}

	h.arch = arch

	return nil
}

func (h *LocalHost) runCommand(s string) (stdout, stderr string, _ error) {
	var bstdout, bstderr bytes.Buffer

	cmd := exec.Command("bash", "-c", s)
	cmd.Stdout = &bstdout
	cmd.Stderr = &bstderr

	e := util.StringError("run command")

	err := cmd.Run()

	h.Log().Debug().
		Str("command", s).
		Str("stdout", bstdout.String()).
		Str("stderr", bstderr.String()).
		Msg("host command finished")

	if err != nil {
		return bstdout.String(), bstderr.String(), e.Wrap(err)
	}

	return bstdout.String(), bstderr.String(), nil
}
