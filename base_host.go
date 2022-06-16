package contest

import (
	"context"
	"debug/elf"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	dockerClient "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/pkg/errors"
	"github.com/spikeekips/mitum/util"
)

var defaultContainerStopTimeout = time.Second

type baseHost struct {
	base        string
	addr        *url.URL
	publishhost string
	client      *dockerClient.Client
	portmaps    *util.LockedMap
	arch        elf.Machine
	user        string
	containers  *util.LockedMap // map[name]cid
}

func newBaseHost(base string, addr *url.URL, client *dockerClient.Client) (*baseHost, error) {
	fmt.Println(">>>>>>>>>>>>>>>>>>>")
	fmt.Println(">>>>>>>>>>>>>>>>>>>", base)
	fmt.Println(">>>>>>>>>>>>>>>>>>>")
	fmt.Println(">>>>>>>>>>>>>>>>>>>")
	fmt.Println(">>>>>>>>>>>>>>>>>>>")
	fmt.Println(">>>>>>>>>>>>>>>>>>>")
	fmt.Println(">>>>>>>>>>>>>>>>>>>")
	fmt.Println(">>>>>>>>>>>>>>>>>>>")
	fmt.Println(">>>>>>>>>>>>>>>>>>>")
	fmt.Println(">>>>>>>>>>>>>>>>>>>")
	fmt.Println(">>>>>>>>>>>>>>>>>>>")
	h := &baseHost{
		base:       filepath.Join(base, util.ULID().String()),
		addr:       addr,
		client:     client,
		portmaps:   util.NewLockedMap(),
		containers: util.NewLockedMap(),
	}

	if err := h.cleanContainers(true); err != nil {
		return nil, errors.Wrap(err, "")
	}

	return h, nil
}

func (h *baseHost) User() string {
	return h.user
}

func (h *baseHost) Base() string {
	return h.base
}

func (h *baseHost) Close() error {
	e := util.StringErrorFunc("failed to close host")

	_ = h.cleanContainers(false)

	if err := h.client.Close(); err != nil {
		return e(err, "")
	}

	return nil
}

func (h *baseHost) cleanContainers(remove bool) error {
	e := util.StringErrorFunc("failed to clean containers")

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

	if len(cids) < 1 {
		return nil
	}

	jobch := make(chan util.ContextWorkerCallback)

	go func() {
		for i := range cids {
			cid := cids[i]
			jobch <- func(ctx context.Context, _ uint64) error {
				_ = h.client.ContainerPause(context.Background(), cid)
				_ = h.client.ContainerStop(context.Background(), cid, &defaultContainerStopTimeout)

				if remove {
					_ = h.client.ContainerRemove(context.Background(), cid, dockerTypes.ContainerRemoveOptions{
						RemoveVolumes: true, Force: true,
					})
				}

				return nil
			}
		}

		close(jobch)
	}()

	if err := util.RunErrgroupWorkerByChan(context.Background(), int64(len(cids)), jobch); err != nil {
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

func (h *baseHost) PublishHost() string {
	if len(h.publishhost) > 0 {
		return h.publishhost
	}

	return h.Hostname()
}

func (h *baseHost) SetPublishHost(s string) {
	h.publishhost = s
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
	_, err := h.containers.Set(name, func(i interface{}) (interface{}, error) {
		if !util.IsNilLockedValue(i) {
			return i, nil
		}

		cid, err := h.createContainer(ctx, config, hostConfig, networkingConfig, name)

		return cid, err
	})

	return err
}

func (h *baseHost) createContainer(
	ctx context.Context,
	config *container.Config,
	hostConfig *container.HostConfig,
	networkingConfig *network.NetworkingConfig,
	name string,
) (string, error) {
	r, err := h.client.ContainerCreate(
		ctx,
		config,
		hostConfig,
		networkingConfig,
		nil,
		name,
	)
	if err != nil {
		return "", errors.Wrap(err, "failed to create container")
	}

	return r.ID, nil
}

func (h *baseHost) StartContainer(
	ctx context.Context,
	config *container.Config,
	hostConfig *container.HostConfig,
	networkingConfig *network.NetworkingConfig,
	name string,
	whenExit func(container.ContainerWaitOKBody, error),
) error {
	e := util.StringErrorFunc("failed to start container")

	cid, err := h.findContainer(ctx, name)
	if err != nil {
		return e(err, "")
	}

	if len(cid) < 1 {
		return e(util.ErrNotFound.Errorf("container not found"), "")
	}

	if err := h.client.ContainerStart(ctx, cid, dockerTypes.ContainerStartOptions{}); err != nil {
		return e(err, "")
	}

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

	cid, err := h.findContainer(ctx, name)
	if err != nil {
		return e(err, "")
	}

	if err := h.client.ContainerPause(ctx, cid); err != nil {
		return e(err, "")
	}

	if timeout == nil {
		timeout = &defaultContainerStopTimeout
	}

	if err := h.client.ContainerStop(ctx, cid, timeout); err != nil {
		return e(err, "")
	}

	return nil
}

func (h *baseHost) RemoveContainer(ctx context.Context, name string, options dockerTypes.ContainerRemoveOptions) error {
	e := util.StringErrorFunc("failed to remove container")

	if err := h.containers.Remove(name, func(i interface{}) error {
		if util.IsNilLockedValue(i) {
			return util.ErrNotFound.Errorf("container not found")
		}

		cid := i.(string)

		if err := h.StopContainer(ctx, name, nil); err != nil {
			return err
		}

		if err := h.client.ContainerRemove(ctx, cid, options); err != nil {
			return err
		}

		return nil
	}); err != nil {
		return e(err, "")
	}

	return nil
}

func (h *baseHost) ContainerLogs(
	ctx context.Context,
	name string,
	options dockerTypes.ContainerLogsOptions,
) (io.ReadCloser, error) {
	e := util.StringErrorFunc("failed container logs")

	cid, err := h.findContainer(ctx, name)
	if err != nil {
		return nil, e(err, "")
	}

	return h.client.ContainerLogs(ctx, cid, options)
}

func (h *baseHost) findContainer(ctx context.Context, name string) (string, error) {
	switch i, found := h.containers.Value(name); {
	case !found:
		return "", nil
	default:
		return i.(string), nil
	}
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

			if p, found := portmap[source]; found {
				port = p[0].HostPort

				return portmap, nil
			}
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
