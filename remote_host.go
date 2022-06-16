package contest

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

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
	sync.Mutex
	*baseHost
	savedsshclient *ssh.Client
}

func NewRemoteHost(base string, dockerhost *url.URL) (*RemoteHost, error) {
	client, err := dockerClient.NewClientWithOpts(
		dockerClient.WithHost(dockerhost.String()),
	)
	if err != nil {
		return nil, errors.Wrap(err, "")
	}

	bh, err := newBaseHost(base, dockerhost, client)
	if err != nil {
		return nil, errors.Wrap(err, "")
	}

	h := &RemoteHost{baseHost: bh}

	if err := h.checkEnv(); err != nil {
		return nil, errors.Wrap(err, "")
	}

	if err := h.checkBase(); err != nil {
		return nil, errors.Wrap(err, "")
	}

	return h, nil
}

func (h *RemoteHost) ContainerFreePort(id, network, innerPort string) (string, error) {
	return h.containerFreePort(id, network, innerPort, func(portmap nat.PortMap) (string, error) {
		session, err := h.sshSession()
		if err != nil {
			return "", errors.Wrap(err, "")
		}

		defer func() {
			_ = session.Close()
		}()

		return h.freePort(session, network, portmap)
	})
}

func (h *RemoteHost) FreePort(network string) (string, error) {
	e := util.StringErrorFunc("failed to get free port")

	session, err := h.sshSession()
	if err != nil {
		return "", e(err, "")
	}

	defer func() {
		_ = session.Close()
	}()

	return h.freePort(session, network, nat.PortMap{})
}

func (h *RemoteHost) Upload(s io.Reader, dest string, mode os.FileMode) error {
	session, err := h.sshSession()
	if err != nil {
		return err
	}

	newdest := filepath.Join(h.base, dest)

	// NOTE golang's sftp is too slow
	session.Stdin = s

	defer func() {
		_ = session.Close()
	}()

	if err := session.Run(fmt.Sprintf(`cat - > '%s'`, newdest)); err != nil {
		return err
	}

	if _, err := h.run(fmt.Sprintf(`chmod 700 '%s'`, newdest)); err != nil {
		return err
	}

	return nil
}

func (h *RemoteHost) CollectResult(outputfile string) error {
	e := util.StringErrorFunc("failed to collect result")

	out, err := os.Create(outputfile)
	if err != nil {
		return e(err, "")
	}

	session, err := h.sshSession()
	if err != nil {
		return e(err, "")
	}

	defer func() {
		_ = session.Close()
	}()

	session.Stdout = out

	if err := session.Run(fmt.Sprintf(`cd "%s" && tar zcf - .`, h.base)); err != nil {
		return e(err, "")
	}

	return nil
}

func (h *RemoteHost) Mkdir(dest string, mode os.FileMode) error {
	e := util.StringErrorFunc("failed to Mkdir")

	client, err := h.sshClient()
	if err != nil {
		return e(err, "")
	}

	st, err := sftp.NewClient(client)
	if err != nil {
		return e(err, "")
	}
	defer func() {
		_ = st.Close()
	}()

	newdest := filepath.Join(h.base, dest)

	if err := st.MkdirAll(newdest); err != nil {
		return e(err, "")
	}

	if err := st.Chmod(newdest, mode); err != nil {
		return e(err, "")
	}

	return nil
}

func (h *RemoteHost) LocalAddr() (addr netip.Addr, _ error) {
	e := util.StringErrorFunc("failed to get local publish address")

	out, err := h.run(`echo "${SSH_CONNECTION}"`)
	if err != nil {
		return addr, e(err, "")
	}

	l := strings.SplitN(out, " ", 2)
	if len(l) != 2 {
		return addr, e(nil, "invalid output")
	}

	addr, err = netip.ParseAddr(l[0])
	if err != nil {
		return addr, e(err, "")
	}

	return addr, nil
}

func (h *RemoteHost) checkEnv() error {
	e := util.StringErrorFunc("failed to check env")

	switch s, err := h.run("id -u"); {
	case err != nil:
		return e(err, "")
	default:
		h.user = strings.TrimSuffix(s, "\n")
	}

	switch s, err := h.run("uname -sm"); {
	case err != nil:
		return e(err, "")
	default:
		uname := strings.TrimSuffix(s, "\n")

		arch, found := supportedArchs[uname]
		if !found {
			return e(nil, "not supported arch, %q", uname)
		}

		h.arch = arch

	}

	return nil
}

func (h *RemoteHost) checkBase() error {
	e := util.StringErrorFunc("failed to check base")

	client, err := h.sshClient()
	if err != nil {
		return e(err, "")
	}

	st, err := sftp.NewClient(client)
	if err != nil {
		return e(err, "")
	}
	defer func() {
		_ = st.Close()
	}()

	if err := st.MkdirAll(h.base); err != nil {
		return e(err, "")
	}

	if err := st.Chmod(h.base, 0o700); err != nil {
		return e(err, "")
	}

	return nil
}

func (h *RemoteHost) sshClient() (*ssh.Client, error) {
	h.Lock()
	defer h.Unlock()

	if h.savedsshclient != nil {
		return h.savedsshclient, nil
	}

	e := util.StringErrorFunc("failed to create ssh client")

	sock, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK"))
	if err != nil {
		return nil, e(err, "")
	}
	agentsock := agent.NewClient(sock)

	signers, err := agentsock.Signers()
	if err != nil {
		return nil, e(err, "")
	}

	config := &ssh.ClientConfig{
		User: os.Getenv("USER"),
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signers...),
		},
		HostKeyCallback: func(string, net.Addr, ssh.PublicKey) error { return nil },
	}

	conn, err := ssh.Dial("tcp", h.Hostname()+":22", config)
	if err != nil {
		return nil, e(err, "")
	}

	h.savedsshclient = conn

	return h.savedsshclient, nil
}

func (h *RemoteHost) sshSession() (*ssh.Session, error) {
	e := util.StringErrorFunc("failed to create ssh session")

	client, err := h.sshClient()
	if err != nil {
		return nil, e(err, "")
	}

	session, err := client.NewSession()
	if err != nil {
		return nil, e(err, "")
	}

	return session, nil
}

func (h *RemoteHost) freePort(session *ssh.Session, network string, portmap nat.PortMap) (string, error) {
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
		return "", errors.Errorf("unsupported network, %q", network)
	}

	return h.baseHost.freePort(network, portmap, func() (string, error) {
		bufstdout.Reset()
		bufstderr.Reset()

		switch err := session.Run(cmd); {
		case err != nil:
			return "", errors.Wrap(err, "")
		case len(bufstderr.Bytes()) > 0:
			return "", errors.Errorf("stderr: %q", bufstderr.String())
		case len(bufstdout.Bytes()) < 1:
			return "", errors.Errorf("empty output")
		default:
			return strings.TrimSpace(bufstdout.String()), nil
		}
	})
}

func (h *RemoteHost) run(cmd string) (string, error) {
	session, err := h.sshSession()
	if err != nil {
		return "", err
	}

	defer func() {
		_ = session.Close()
	}()

	var b bytes.Buffer
	session.Stdout = &b

	if err := session.Run(cmd); err != nil {
		return "", err
	}

	return b.String(), nil
}
