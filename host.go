package contest

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
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

type Hosts struct {
	sync.Mutex
	hostids          []string
	hosts            map[string]Host
	hostsbycontainer map[string]Host
	containersbyhost map[string][]string
	lastused         int
}

func NewHosts() *Hosts {
	return &Hosts{
		hosts:            map[string]Host{},
		hostsbycontainer: map[string]Host{},
		containersbyhost: map[string][]string{},
		lastused:         -1,
	}
}

func (h *Hosts) Close() error {
	for i := range h.hosts {
		if err := h.hosts[i].Close(); err != nil {
			return err
		}
	}

	return nil
}

func (h *Hosts) New(ho Host) error {
	if _, found := h.hosts[ho.Address()]; found {
		return errors.Errorf("already added")
	}

	h.hostids = append(h.hostids, ho.Address())
	h.hosts[ho.Address()] = ho

	return nil
}

func (h *Hosts) NewContainer(cid string) (Host, error) {
	ho := h.findHost()
	if ho == nil {
		return nil, errors.Errorf("failed to find host")
	}

	h.hostsbycontainer[cid] = ho
	h.containersbyhost[ho.Address()] = append(h.containersbyhost[ho.Address()], cid)

	return ho, nil
}

func (h *Hosts) Host(hostaddress string) Host {
	return h.hosts[hostaddress]
}

func (h *Hosts) HostByContainer(cid string) Host {
	return h.hostsbycontainer[cid]
}

func (h *Hosts) TraverseByHost(f func(_ Host, cids []string) bool) {
	for addr := range h.containersbyhost {
		ho := h.Host(addr)

		if !f(ho, h.containersbyhost[addr]) {
			break
		}
	}
}

func (h *Hosts) findHost() Host {
	h.Lock()
	defer h.Unlock()

	if len(h.hostids) < 1 {
		return nil
	}

	index := h.lastused + 1
	if index == len(h.hostids) {
		index = 0
	}

	h.lastused = index

	return h.hosts[h.hostids[index]]
}

type Host interface {
	Address() string
	Hostname() string
	Close() error
	Client() *dockerClient.Client
	Upload(io.Reader, string, os.FileMode) error
	ContainerFreePort(string, string, string) (string, error)
	CreateContainer(
		_ context.Context,
		_ *container.Config,
		_ *container.HostConfig,
		_ *network.NetworkingConfig,
		containerName string,
	) error
	StartContainer(
		_ context.Context,
		_ *container.Config,
		_ *container.HostConfig,
		_ *network.NetworkingConfig,
		containerName string,
	) error
	StopContainer(_ context.Context, containerName string, _ *time.Duration) error
	RemoveContainer(_ context.Context, containerName string, _ dockerTypes.ContainerRemoveOptions) error
	ContainerLogs(_ context.Context, containerName string, _ types.ContainerLogsOptions) (io.ReadCloser, error)
	PortMap(string) nat.PortMap
	FreePort(string) (string, error)
}

type RemoteDockerHost struct {
	sync.RWMutex
	addr           *url.URL
	client         *dockerClient.Client
	started        map[ /* container name */ string] /* container id */ string
	stopped        map[ /* container name */ string] /* container id */ string
	savedsshclient *ssh.Client
	portmaps       *util.LockedMap
}

func NewRemoteDockerHost(addr *url.URL) (*RemoteDockerHost, error) {
	client, err := dockerClient.NewClientWithOpts(
		dockerClient.WithHost(addr.String()),
	)
	if err != nil {
		return nil, errors.Wrap(err, "")
	}

	// NOTE check ssh connection
	return &RemoteDockerHost{
		addr:     addr,
		client:   client,
		started:  map[string]string{},
		stopped:  map[string]string{},
		portmaps: util.NewLockedMap(),
	}, nil
}

func (h *RemoteDockerHost) Address() string {
	return h.addr.String()
}

func (h *RemoteDockerHost) Hostname() string {
	return h.addr.Hostname()
}

func (h *RemoteDockerHost) Close() error {
	e := util.StringErrorFunc("failed to close host")

	l, err := h.client.ContainerList(context.Background(), dockerTypes.ContainerListOptions{All: true})
	if err != nil {
		return e(err, "")
	}

	var cids []string
	for i := range l {
		c := l[i]

		for j := range c.Names {
			if strings.HasPrefix(c.Names[j], "/"+ContainerLabel) {
				cids = append(cids, c.ID)
			}
		}
	}

	if len(cids) > 0 {
		for i := range cids {
			if err := h.client.ContainerStop(context.Background(), cids[i], nil); err != nil {
				return e(err, "")
			}
		}
	}

	if err := h.client.Close(); err != nil {
		return e(err, "")
	}

	return nil
}

func (h *RemoteDockerHost) ContainerFreePort(id, network, innerPort string) (string, error) {
	var port string
	if _, err := h.portmaps.Set(id, func(i interface{}) (interface{}, error) {
		source, err := nat.NewPort(network, innerPort)
		if err != nil {
			return nil, err
		}

		if port, err = h.FreePort(network); err != nil {
			return nil, err
		}

		var portmap nat.PortMap
		switch {
		case i == nil, util.IsNilLockedValue(i):
			portmap = nat.PortMap{}
		default:
			portmap = i.(nat.PortMap)
		}

		portmap[source] = []nat.PortBinding{{HostPort: port}}

		return portmap, nil
	}); err != nil {
		return port, errors.Wrap(err, "failed to get free port of node")
	}

	return port, nil
}

func (h *RemoteDockerHost) PortMap(id string) nat.PortMap {
	i, _ := h.portmaps.Value(id)
	if i == nil {
		return nat.PortMap{}
	}

	return i.(nat.PortMap)
}

func (h *RemoteDockerHost) FreePort(network string) (string, error) {
	e := util.StringErrorFunc("failed to get free port")

	session, err := h.sshSession()
	if err != nil {
		return "", e(err, "")
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
		return "", e(nil, "unsupported network, %q", network)
	}

	switch err := session.Run(cmd); {
	case err != nil:
		return "", e(err, "")
	case len(bufstderr.Bytes()) > 0:
		return "", e(nil, bufstderr.String())
	case len(bufstdout.Bytes()) < 1:
		return "", e(nil, "empty output")
	default:
		return strings.TrimSpace(bufstdout.String()), nil
	}
}

func (h *RemoteDockerHost) Client() *dockerClient.Client {
	return h.client
}

func (h *RemoteDockerHost) Upload(s io.Reader, dest string, mode os.FileMode) error {
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
	defer f.Close()

	if _, err := f.ReadFrom(s); err != nil {
		return e(err, "")
	}

	if err := st.Chmod(dest, mode); err != nil {
		return e(err, "")
	}

	return nil
}

func (h *RemoteDockerHost) CreateContainer(
	ctx context.Context,
	config *container.Config,
	hostConfig *container.HostConfig,
	networkingConfig *network.NetworkingConfig,
	name string,
) error {
	h.Lock()
	defer h.Unlock()

	_, err := h.createContainer(ctx, config, hostConfig, networkingConfig, name)

	return err
}

func (h *RemoteDockerHost) createContainer(
	ctx context.Context,
	config *container.Config,
	hostConfig *container.HostConfig,
	networkingConfig *network.NetworkingConfig,
	name string,
) (string, error) {
	e := util.StringErrorFunc("failed to create container")

	cid, _, err := h.findContainer(ctx, name)
	if err != nil {
		return "", e(err, "")
	}

	if len(cid) < 1 {
		r, err := h.client.ContainerCreate(
			ctx,
			config,
			hostConfig,
			networkingConfig,
			nil,
			name,
		)
		if err != nil {
			return "", e(err, "")
		}

		cid = r.ID
	}

	h.stopped[name] = cid

	return cid, nil
}

func (h *RemoteDockerHost) StartContainer(
	ctx context.Context,
	config *container.Config,
	hostConfig *container.HostConfig,
	networkingConfig *network.NetworkingConfig,
	name string,
) error {
	h.Lock()
	defer h.Unlock()

	e := util.StringErrorFunc("failed to start container")

	cid, started, err := h.findContainer(ctx, name)
	if err != nil {
		return e(err, "")
	}

	if len(cid) < 1 {
		id, err := h.createContainer(
			ctx,
			config,
			hostConfig,
			networkingConfig,
			name,
		)
		if err != nil {
			return e(err, "")
		}

		cid = id
	}

	if !started {
		if err := h.client.ContainerStart(ctx, cid, dockerTypes.ContainerStartOptions{}); err != nil {
			return e(err, "")
		}
	}

	h.started[name] = cid
	delete(h.stopped, name)

	return nil
}

func (h *RemoteDockerHost) StopContainer(ctx context.Context, name string, timeout *time.Duration) error {
	e := util.StringErrorFunc("failed to stop container")

	cid, started, err := h.findContainer(ctx, name)
	if err != nil {
		return e(err, "")
	}

	if started {
		if err := h.client.ContainerStop(ctx, cid, timeout); err != nil {
			return e(err, "")
		}
	}

	h.stopped[name] = cid
	delete(h.started, name)

	return nil
}

func (h *RemoteDockerHost) RemoveContainer(ctx context.Context, name string, options dockerTypes.ContainerRemoveOptions) error {
	e := util.StringErrorFunc("failed to remove container")

	cid, started, err := h.findContainer(ctx, name)
	if err != nil {
		return e(err, "")
	}

	if len(cid) < 1 {
		return e(util.ErrNotFound.Errorf("container not found"), "")
	}

	if started {
		if err := h.client.ContainerStop(ctx, cid, nil); err != nil {
			return e(err, "")
		}
	}

	if err := h.client.ContainerRemove(ctx, cid, options); err != nil {
		return e(err, "")
	}

	delete(h.stopped, name)
	delete(h.started, name)

	return nil
}

func (h *RemoteDockerHost) ContainerLogs(
	ctx context.Context,
	name string,
	options dockerTypes.ContainerLogsOptions,
) (io.ReadCloser, error) {
	e := util.StringErrorFunc("failed container logs")

	var cid string

	switch id, found := h.stopped[name]; {
	case found:
		cid = id
	default:
		switch id, found := h.started[name]; {
		case !found:
			return nil, e(nil, "container not found")
		default:
			cid = id
		}
	}

	return h.client.ContainerLogs(ctx, cid, options)
}

func (h *RemoteDockerHost) sshClient() (*ssh.Client, error) {
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

func (h *RemoteDockerHost) sshSession() (*ssh.Session, error) {
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

func (h *RemoteDockerHost) findContainer(ctx context.Context, name string) (string, bool, error) {
	l, err := h.client.ContainerList(ctx, dockerTypes.ContainerListOptions{All: true})
	if err != nil {
		return "", false, err
	}

	var cid string
	var started bool

	for i := range l {
		c := l[i]

		if util.InStringSlice("/"+name, c.Names) {
			cid = c.ID

			started = c.State == "running"

			break
		}
	}

	return cid, started, nil
}
