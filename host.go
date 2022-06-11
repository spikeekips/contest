package contest

import (
	"context"
	"io"
	"os"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	dockerClient "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/pkg/errors"
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

func (h *Hosts) TraverseByHost(f func(_ Host, cids []string) (bool, error)) error {
	for addr := range h.containersbyhost {
		ho := h.Host(addr)

		switch keep, err := f(ho, h.containersbyhost[addr]); {
		case err != nil:
			return err
		case !keep:
			return nil
		}
	}

	return nil
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
		whenExit func(container.ContainerWaitOKBody, error),
	) error
	StopContainer(_ context.Context, containerName string, _ *time.Duration) error
	RemoveContainer(_ context.Context, containerName string, _ dockerTypes.ContainerRemoveOptions) error
	ContainerLogs(_ context.Context, containerName string, _ types.ContainerLogsOptions) (io.ReadCloser, error)
	PortMap(string) nat.PortMap
	FreePort(string) (string, error)
}
