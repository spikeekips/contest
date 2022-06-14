package contest

import (
	"context"
	"debug/elf"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	dockerClient "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/pkg/errors"
	"github.com/spikeekips/mitum/util"
)

type baseHost struct {
	sync.RWMutex
	addr     *url.URL
	client   *dockerClient.Client
	started  map[ /* container name */ string] /* container id */ string
	stopped  map[ /* container name */ string] /* container id */ string
	portmaps *util.LockedMap
	arch     elf.Machine
}

func newBaseHost(addr *url.URL, client *dockerClient.Client) *baseHost {
	return &baseHost{
		addr:     addr,
		client:   client,
		started:  map[string]string{},
		stopped:  map[string]string{},
		portmaps: util.NewLockedMap(),
	}
}

func (h *baseHost) Close() error {
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

func (h *baseHost) Arch() elf.Machine {
	return h.arch
}

func (h *baseHost) Address() string {
	return h.addr.String()
}

func (h *baseHost) Hostname() string {
	return h.addr.Hostname()
}

func (h *baseHost) PortMap(id string) nat.PortMap {
	i, _ := h.portmaps.Value(id)
	if i == nil {
		return nat.PortMap{}
	}

	return i.(nat.PortMap)
}

func (h *baseHost) Client() *dockerClient.Client {
	return h.client
}

func (h *baseHost) CreateContainer(
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

func (h *baseHost) createContainer(
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

func (h *baseHost) StartContainer(
	ctx context.Context,
	config *container.Config,
	hostConfig *container.HostConfig,
	networkingConfig *network.NetworkingConfig,
	name string,
	whenExit func(container.ContainerWaitOKBody, error),
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

	if whenExit != nil {
		go func() {
			bodych, errch := h.client.ContainerWait(ctx, cid, container.WaitConditionNotRunning)

			select {
			case err := <-errch:
				whenExit(container.ContainerWaitOKBody{}, err)

				return
			case body := <-bodych:
				whenExit(body, nil)

				return
			}
		}()
	}

	return nil
}

func (h *baseHost) StopContainer(ctx context.Context, name string, timeout *time.Duration) error {
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

func (h *baseHost) RemoveContainer(ctx context.Context, name string, options dockerTypes.ContainerRemoveOptions) error {
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

func (h *baseHost) ContainerLogs(
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

func (h *baseHost) findContainer(ctx context.Context, name string) (string, bool, error) {
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

func (h *baseHost) containerFreePort(
	id, network, innerPort string,
	f func(nat.PortMap) (port string, _ error),
) (string, error) {
	e := util.StringErrorFunc("failed to get free container port")

	var port string
	if _, err := h.portmaps.Set(id, func(i interface{}) (interface{}, error) {
		source, err := nat.NewPort(network, innerPort)
		if err != nil {
			return nil, err
		}

		var portmap nat.PortMap
		switch {
		case i == nil, util.IsNilLockedValue(i):
			portmap = nat.PortMap{}
		default:
			portmap = i.(nat.PortMap)
		}

		if port, err = f(portmap); err != nil {
			return nil, err
		}

		portmap[source] = []nat.PortBinding{{HostPort: port}}

		return portmap, nil
	}); err != nil {
		return port, e(err, "failed to get free port of node")
	}

	return port, nil
}

func (h *baseHost) freePort(
	network string,
	portmap nat.PortMap,
	f func() (port string, _ error),
) (string, error) {
	for {
		port, err := f()
		if err != nil {
			return "", errors.Wrap(err, "")
		}

		var found bool

	end:
		for i := range portmap {
			for j := range portmap[i] {
				if portmap[i][j].HostPort == port {
					found = true

					break end
				}
			}
		}

		if !found {
			return port, nil
		}
	}
}
