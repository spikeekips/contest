package contest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	dockerClient "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/pkg/errors"
	"github.com/pkg/sftp"
	"github.com/spikeekips/mitum/util"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

var udpFreeportCmdF = `read FROM TO < /proc/sys/net/ipv4/ip_local_port_range
		comm -23 \
		<(seq "$FROM" "$TO" | sort) \
		<(ss -Huan | awk '{print $4}' | cut -d':' -f2 | sort -u) | \
		shuf | head -n 1`

var tcpFreeportCmdF = `read FROM TO < /proc/sys/net/ipv4/ip_local_port_range
		comm -23 \
		<(seq "$FROM" "$TO" | sort) \
		<(ss -Htan | awk '{print $4}' | cut -d':' -f2 | sort -u) | \
		shuf | head -n 1`

type RemoteHost struct {
	savedsshconn ssh.Conn
	*baseHost
	savedsshclient *ssh.Client
	sync.Mutex
}

func NewRemoteHost(base string, dockerhost *url.URL) (*RemoteHost, error) {
	client, err := dockerClient.NewClientWithOpts(
		dockerClient.WithHost(dockerhost.String()),
	)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	bh, err := newBaseHost(base, dockerhost, client)
	if err != nil {
		return nil, err
	}

	h := &RemoteHost{baseHost: bh}

	if err := h.checkEnv(); err != nil {
		return nil, err
	}

	if err := h.checkBase(); err != nil {
		return nil, err
	}

	return h, nil
}

func (h *RemoteHost) Close() error {
	if h.savedsshconn != nil {
		_ = h.savedsshconn.Close()
	}

	return h.baseHost.Close()
}

func (h *RemoteHost) FreePort(id, network string) (string, error) {
	return h.baseHost.freePort(id, network, func(network string) (string, error) {
		return h.remoteFreePort(network, nat.PortMap{})
	})
}

func (h *RemoteHost) Upload(s io.Reader, name, dest string, mode os.FileMode) error {
	e := util.StringError("upload file")

	newdest := filepath.Join(h.base, dest)

	var origr io.ReadSeeker
	var buf bytes.Buffer

	nr := io.TeeReader(s, &buf)

	if err := util.Retry(context.Background(), func() (bool, error) {
		if origr != nil {
			if _, err := origr.Seek(0, 0); err != nil {
				return false, errors.WithStack(err)
			}

			nr = origr
		}

		if err := h.upload(nr, name, newdest); err != nil {
			if origr == nil {
				_, _ = io.ReadAll(nr)

				origr = bytes.NewReader(buf.Bytes())
			}

			return true, err
		}

		return false, nil
	}, 3, time.Second); err != nil { //nolint:gomnd //...
		return e.Wrap(err)
	}

	if _, _, err := h.runCommand(fmt.Sprintf(`chmod %o '%s'`, mode, newdest)); err != nil {
		return e.Wrap(err)
	}

	h.addFile(name, newdest)

	return nil
}

func (h *RemoteHost) upload(s io.Reader, _, dest string) error {
	session, err := h.sshSession()
	if err != nil {
		return err
	}

	defer func() {
		_ = session.Close()
	}()

	// NOTE golang's sftp is too slow
	stdinw, err := session.StdinPipe()
	if err != nil {
		return errors.WithStack(err)
	}

	errch := make(chan error, 2)

	go func() {
		_, err := io.Copy(stdinw, s)
		_ = stdinw.Close()

		if errors.Is(err, io.EOF) {
			err = nil
		}

		errch <- err
	}()

	if err := session.Run(fmt.Sprintf(`cat > '%s'`, dest)); err != nil {
		var ssherr *ssh.ExitMissingError

		if !errors.As(err, &ssherr) {
			return errors.WithStack(err)
		}
	}

	return <-errch
}

func (h *RemoteHost) CollectResult(outputfile string) error {
	e := util.StringError("collect result")

	out, err := os.Create(outputfile)
	if err != nil {
		return e.Wrap(err)
	}

	session, err := h.sshSession()
	if err != nil {
		return e.Wrap(err)
	}

	defer func() {
		_ = session.Close()
	}()

	session.Stdout = out

	if err := session.Run(fmt.Sprintf(`cd "%s" && tar zcf - .`, h.base)); err != nil {
		return e.Wrap(err)
	}

	return nil
}

func (h *RemoteHost) Mkdir(dest string, mode os.FileMode) error {
	e := util.StringError("Mkdir")

	client, err := h.sshClient()
	if err != nil {
		return e.Wrap(err)
	}

	st, err := sftp.NewClient(client)
	if err != nil {
		return e.Wrap(err)
	}

	defer func() {
		_ = st.Close()
	}()

	newdest := filepath.Join(h.base, dest)

	if err := st.MkdirAll(newdest); err != nil {
		return e.Wrap(err)
	}

	if err := st.Chmod(newdest, mode); err != nil {
		return e.Wrap(err)
	}

	return nil
}

func (h *RemoteHost) LocalAddr() (addr netip.Addr, _ error) {
	e := util.StringError("get local publish address")

	out, _, err := h.runCommand(`echo "${SSH_CONNECTION}"`)
	if err != nil {
		return addr, e.Wrap(err)
	}

	l := strings.SplitN(out, " ", 2)
	if len(l) != 2 { //nolint:gomnd //...
		return addr, e.Errorf("invalid output")
	}

	addr, err = netip.ParseAddr(l[0])
	if err != nil {
		return addr, e.Wrap(err)
	}

	return addr, nil
}

func (h *RemoteHost) checkEnv() error {
	e := util.StringError("check env")

	switch s, _, err := h.runCommand("id -u"); {
	case err != nil:
		return e.Wrap(err)
	default:
		h.user = strings.TrimSuffix(s, "\n")
	}

	switch s, _, err := h.runCommand("uname -sm"); {
	case err != nil:
		return e.Wrap(err)
	default:
		uname := strings.TrimSuffix(s, "\n")

		arch, found := supportedArchs[uname]
		if !found {
			return e.Errorf("not supported arch, %q", uname)
		}

		h.arch = arch
	}

	return nil
}

func (h *RemoteHost) checkBase() error {
	e := util.StringError("check base")

	client, err := h.sshClient()
	if err != nil {
		return e.Wrap(err)
	}

	st, err := sftp.NewClient(client)
	if err != nil {
		return e.Wrap(err)
	}

	defer func() {
		_ = st.Close()
	}()

	if err := st.MkdirAll(h.base); err != nil {
		return e.Wrap(err)
	}

	if err := st.Chmod(h.base, 0o700); err != nil {
		return e.Wrap(err)
	}

	return nil
}

func (h *RemoteHost) sshClient() (*ssh.Client, error) {
	h.Lock()
	defer h.Unlock()

	if h.savedsshclient != nil {
		return h.savedsshclient, nil
	}

	return h.newSSHClient()
}

func (h *RemoteHost) newSSHClient() (*ssh.Client, error) {
	e := util.StringError("create ssh client")

	sock, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK"))
	if err != nil {
		return nil, e.Wrap(err)
	}

	agentsock := agent.NewClient(sock)

	signers, err := agentsock.Signers()
	if err != nil {
		return nil, e.Wrap(err)
	}

	config := &ssh.ClientConfig{
		User: os.Getenv("USER"),
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signers...),
		},
		HostKeyCallback: func(string, net.Addr, ssh.PublicKey) error { return nil },
		Timeout:         0,
	}

	addr := h.Hostname() + ":22"

	netconn, err := net.DialTimeout("tcp", addr, time.Second*10) //nolint:gomnd //...
	if err != nil {
		return nil, errors.WithStack(err)
	}

	conn, chans, reqs, err := ssh.NewClientConn(netconn, addr, config)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	h.savedsshclient = ssh.NewClient(conn, chans, reqs)
	h.savedsshconn = conn

	return h.savedsshclient, nil
}

func (h *RemoteHost) sshSession() (*ssh.Session, error) {
	e := util.StringError("create ssh session")

	client, err := h.sshClient()
	if err != nil {
		return nil, e.Wrap(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3) //nolint:gomnd //...
	defer cancel()

	var session *ssh.Session

	if err := util.Retry(ctx, func() (_ bool, err error) {
		session, err = client.NewSession()

		var ssherr *net.OpError
		switch {
		case err == nil:
			return false, nil
		case errors.As(err, &ssherr):
			client, err = func() (*ssh.Client, error) {
				h.Lock()
				defer h.Unlock()

				_ = h.savedsshconn.Close()

				return h.newSSHClient()
			}()

			return true, err
		default:
			return true, err
		}
	}, -1, time.Millisecond*600); err != nil { //nolint:gomnd //...
		return nil, e.Wrap(err)
	}

	return session, nil
}

func (h *RemoteHost) remoteFreePort(network string, _ nat.PortMap) (string, error) {
	e := util.StringError("get free port")

	session, err := h.sshSession()
	if err != nil {
		return "", e.Wrap(err)
	}

	defer func() {
		_ = session.Close()
	}()

	var bufstdout, bufstderr bytes.Buffer
	session.Stdout = &bufstdout
	session.Stderr = &bufstderr

	var cmd string

	switch network {
	case "udp":
		cmd = udpFreeportCmdF
	case "tcp":
		cmd = tcpFreeportCmdF
	default:
		return "", e.Errorf("unsupported network, %q", network)
	}

	bufstdout.Reset()
	bufstderr.Reset()

	switch err := session.Run(cmd); {
	case err != nil:
		return "", e.Wrap(err)
	case len(bufstderr.Bytes()) > 0:
		return "", e.Errorf("stderr: %q", bufstderr.String())
	case len(bufstdout.Bytes()) < 1:
		return "", e.Errorf("empty output")
	default:
		return strings.TrimSpace(bufstdout.String()), nil
	}
}

func (h *RemoteHost) RunCommand(cmd string) (stdout string, stderr string, ok bool, err error) {
	var e *exec.ExitError

	switch stdout, stderr, err = h.runCommand(cmd); {
	case err == nil:
		return stdout, stderr, true, nil
	case errors.As(err, &e):
		return stdout, stderr, false, nil
	default:
		return stdout, stderr, false, errors.WithStack(err)
	}
}

func (h *RemoteHost) runCommand(cmd string) (stdout string, stderr string, _ error) {
	session, err := h.sshSession()
	if err != nil {
		return "", "", err
	}

	defer func() {
		_ = session.Close()
	}()

	var bstdout, bstderr bytes.Buffer

	session.Stdout = &bstdout
	session.Stderr = &bstderr

	err = session.Run(cmd)

	h.Log().Debug().Str("stdout", bstdout.String()).Str("stderr", bstderr.String()).Msg("host command finished")

	return bstdout.String(), bstderr.String(), err
}
