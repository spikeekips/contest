package contest

import (
	"context"
	"debug/elf"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	dockerClient "github.com/docker/docker/client"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/spikeekips/mitum/util"
	"github.com/spikeekips/mitum/util/logging"
)

var defaultContainerStopTimeout = time.Second

type baseHost struct {
	*logging.Logging
	containers  *util.SingleLockedMap[string, string]
	addr        *url.URL
	files       map[string]string
	ports       map[string]string
	client      *dockerClient.Client
	user        string
	base        string
	publishhost string
	arch        elf.Machine
	portsLock   sync.Mutex
}

func newBaseHost(base string, addr *url.URL, client *dockerClient.Client) (*baseHost, error) {
	h := &baseHost{
		Logging: logging.NewLogging(func(zctx zerolog.Context) zerolog.Context {
			return zctx.Str("module", "host").Stringer("addr", addr)
		}),
		base:       filepath.Join(base, util.ULID().String()),
		addr:       addr,
		client:     client,
		containers: util.NewSingleLockedMap[string, string](),
		ports:      map[string]string{},
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
	e := util.StringError("close host")

	_ = h.cleanContainers(false)

	if err := h.client.Close(); err != nil {
		return e.Wrap(err)
	}

	return nil
}

func (h *baseHost) cleanContainers(remove bool) error { //revive:disable-line:flag-parameter
	e := util.StringError("clean containers")

	l, err := h.client.ContainerList(context.Background(), container.ListOptions{All: true})
	if err != nil {
		return e.Wrap(err)
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

	n := int64(len(cids))

	if n < 1 {
		return nil
	}

	if err := util.RunJobWorker(
		context.Background(),
		n, n,
		func(_ context.Context, i, _ uint64) error {
			cid := cids[i]

			timeout := int(defaultContainerStopTimeout.Seconds())

			_ = h.client.ContainerPause(context.Background(), cid)
			_ = h.client.ContainerStop(context.Background(), cid, container.StopOptions{Timeout: &timeout})

			if remove {
				_ = h.client.ContainerRemove(context.Background(), cid, container.RemoveOptions{
					RemoveVolumes: true, Force: true,
				})
			}

			return nil
		},
	); err != nil {
		return e.Wrap(err)
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
	if h.publishhost != "" {
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
	cid, found = h.containers.Value(name)
	if !found {
		return cid, info, false, nil
	}

	l, err := h.client.ContainerList(ctx, container.ListOptions{All: true})
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
	_, _, err := h.containers.Set(name, func(i string, found bool) (string, error) {
		if found {
			return i, nil
		}

		return h.createContainer(ctx, config, hostConfig, networkingConfig, name)
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
		return "", errors.Wrap(err, "create container")
	}

	return r.ID, nil
}

func (h *baseHost) StartContainer(
	ctx context.Context,
	_ *container.HostConfig,
	_ *network.NetworkingConfig,
	name string,
	whenExit func(container.WaitResponse, error),
) error {
	e := util.StringError("start container")

	cid, err := h.findContainer(ctx, name)
	if err != nil {
		return e.Wrap(err)
	}

	if err := h.client.ContainerStart(ctx, cid, container.StartOptions{}); err != nil {
		return e.Wrap(err)
	}

	if whenExit != nil {
		go func() {
			bodych, errch := h.client.ContainerWait(ctx, cid, container.WaitConditionNotRunning)

			select {
			case err := <-errch:
				whenExit(container.WaitResponse{}, err)

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
	e := util.StringError("stop container")

	cid, err := h.findContainer(ctx, name)
	if err != nil {
		return e.Wrap(err)
	}

	if err := h.client.ContainerPause(ctx, cid); err != nil {
		return e.Wrap(err)
	}

	var ntimeout int

	switch {
	case timeout == nil:
		ntimeout = int(defaultContainerStopTimeout.Seconds())
	default:
		ntimeout = int(timeout.Seconds())
	}

	if err := h.client.ContainerStop(ctx, cid, container.StopOptions{Timeout: &ntimeout}); err != nil {
		return e.Wrap(err)
	}

	return nil
}

func (h *baseHost) RemoveContainer(ctx context.Context, name string, options container.RemoveOptions) error {
	e := util.StringError("remove container")

	if _, err := h.containers.Remove(name, func(i string, found bool) error {
		if !found {
			return nil
		}

		if err := h.client.ContainerRemove(ctx, i, options); err != nil {
			return errors.WithStack(err)
		}

		return nil
	}); err != nil {
		return e.Wrap(err)
	}

	return nil
}

func (h *baseHost) ContainerLogs(
	ctx context.Context,
	name string,
	options container.LogsOptions,
) (io.ReadCloser, error) {
	e := util.StringError("failed container logs")

	cid, err := h.findContainer(ctx, name)
	if err != nil {
		return nil, e.Wrap(err)
	}

	r, err := h.client.ContainerLogs(ctx, cid, options)
	if err != nil {
		return nil, e.Wrap(err)
	}

	return r, nil
}

func (h *baseHost) findContainer(_ context.Context, name string) (string, error) {
	switch i, found := h.containers.Value(name); {
	case !found:
		return "", util.ErrNotFound.Errorf("container not found")
	default:
		return i, nil
	}
}

func (h *baseHost) freePort(
	id, n string,
	f func(n string) (port string, _ error),
) (string, error) {
	h.portsLock.Lock()
	defer h.portsLock.Unlock()

	if p, found := h.ports[id]; found {
		return p, nil
	}

	ports := map[string]struct{}{}

	for i := range h.ports {
		ports[h.ports[i]] = struct{}{}
	}

	for {
		p, err := f(n)
		if err != nil {
			return "", err
		}

		if _, found := ports[p]; !found {
			h.ports[id] = p

			return p, nil
		}
	}
}

func (h *baseHost) addFile(name, path string) {
	h.files[name] = path
}

func (h *baseHost) File(name string) (string, bool) {
	path, found := h.files[name]

	return path, found
}
