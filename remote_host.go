package contest

import (
	"bytes"
	"io"
	"net"
	"net/url"
	"os"
	"strings"

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
	*baseHost
	savedsshclient *ssh.Client
}

func NewRemoteHost(addr *url.URL) (*RemoteHost, error) {
	client, err := dockerClient.NewClientWithOpts(
		dockerClient.WithHost(addr.String()),
	)
	if err != nil {
		return nil, errors.Wrap(err, "")
	}

	h := &RemoteHost{baseHost: newBaseHost(addr, client)}

	if err := h.checkArch(); err != nil {
		return nil, errors.Wrap(err, "")
	}

	return h, nil
}

func (h *RemoteHost) checkArch() error {
	e := util.StringErrorFunc("failed to check arch")

	session, err := h.sshSession()
	if err != nil {
		return e(err, "")
	}
	defer func() {
		_ = session.Close()
	}()

	var b bytes.Buffer
	session.Stdout = &b

	if err := session.Run("uname -sm"); err != nil {
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

func (h *RemoteHost) ContainerFreePort(id, network, innerPort string) (string, error) {
	session, err := h.sshSession()
	if err != nil {
		return "", errors.Wrap(err, "")
	}

	defer func() {
		_ = session.Close()
	}()

	return h.containerFreePort(id, network, innerPort, func(portmap nat.PortMap) (string, error) {
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

func (h *RemoteHost) Upload(s io.Reader, dest string, mode os.FileMode) error {
	e := util.StringErrorFunc("failed to sftp")

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

	f, err := st.Create(dest)
	if err != nil {
		return e(err, "")
	}

	defer func() {
		_ = f.Close()
	}()

	if _, err := f.ReadFrom(s); err != nil {
		return e(err, "")
	}

	if err := st.Chmod(dest, mode); err != nil {
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

	conn, err := ssh.Dial("tcp", h.addr.Hostname()+":22", config)
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
