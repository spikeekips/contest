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
	"github.com/pkg/errors"
	"github.com/spikeekips/mitum/util"
)

var defaultContainerStopTimeout = time.Second

type baseHost struct {
	containers  *util.LockedMap
	addr        *url.URL
	files       map[string]string
	client      *dockerClient.Client
	ports       *util.LockedMap
	user        string
	base        string
	publishhost string
	arch        elf.Machine
}

func newBaseHost(base string, addr *url.URL, client *dockerClient.Client) (*baseHost, error) {
	h := &baseHost{
		base:       filepath.Join(base, util.ULID().String()),
		addr:       addr,
		client:     client,
		containers: util.NewLockedMap(),
		ports:      util.NewLockedMap(),
		files:      map[string]string{},
	}

	if err := h.cleanContainers(true); err != nil {
		return nil, err
	}

	return h, nil
}

func (h *baseHost) HostID() string {
	return fmt.Sprintf("%s-%s", h.Hostname(), filepath.Base(h.base))
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

func (h *baseHost) cleanContainers(remove bool) error { //revive:disable-line:flag-parameter
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

	if err := util.RunErrgroupWorker(
		context.Background(),
		uint64(len(cids)),
		func(ctx context.Context, i, _ uint64) error {
			cid := cids[i]

			_ = h.client.ContainerPause(context.Background(), cid)
			_ = h.client.ContainerStop(context.Background(), cid, &defaultContainerStopTimeout)

			if remove {
				_ = h.client.ContainerRemove(context.Background(), cid, dockerTypes.ContainerRemoveOptions{
					RemoveVolumes: true, Force: true,
				})
			}

			return nil
		},
	); err != nil {
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

func (h *baseHost) Client() *dockerClient.Client {
	return h.client
}

func (h *baseHost) ExistsContainer(ctx context.Context, name string) (cid, info string, found bool, _ error) {
	i, found := h.containers.Value(name)
	if !found {
		return cid, info, false, nil
	}

	cid = i.(string) //nolint:forcetypeassert //...

	l, err := h.client.ContainerList(ctx, dockerTypes.ContainerListOptions{All: true})
	if err != nil {
		return cid, info, false, errors.WithStack(err)
	}

end:
	for i := range l {
		c := l[i]

		for j := range c.Names {
			if strings.HasPrefix(c.Names[j], "/"+name) {
				info = c.State // NOTE one of "created", "running", "paused",
				// "restarting", "removing", "exited", or "dead"

				found = true

				break end
			}
		}
	}

	return cid, info, found, nil
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

		if err := h.client.ContainerRemove(ctx, i.(string), options); err != nil { //nolint:forcetypeassert //...
			return errors.WithStack(err)
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

	r, err := h.client.ContainerLogs(ctx, cid, options)
	if err != nil {
		return nil, e(err, "")
	}

	return r, nil
}

func (h *baseHost) findContainer(ctx context.Context, name string) (string, error) {
	switch i, found := h.containers.Value(name); {
	case !found:
		return "", util.ErrNotFound.Errorf("container not found")
	default:
		return i.(string), nil //nolint:forcetypeassert //...
	}
}

func (h *baseHost) freePort(
	id, n string,
	f func(n string) (port string, _ error),
) (string, error) {
	i, _, err := h.ports.Get(id, func() (interface{}, error) {
		return f(n)
	})
	if err != nil {
		return "", err
	}

	return i.(string), nil //nolint:forcetypeassert //...
}

func (h *baseHost) addFile(name string, path string) {
	h.files[name] = path
}

func (h *baseHost) File(name string) (string, bool) {
	path, found := h.files[name]

	return path, found
}
