package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	dockerMount "github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/strslice"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	contest "github.com/spikeekips/contest2"
	"github.com/spikeekips/mitum/util"
	"go.mongodb.org/mongo-driver/bson"
)

type nodeInfo struct {
	host          contest.Host
	alias         string
	debugHTTPPort string
}

func (i nodeInfo) MarshalZerologObject(e *zerolog.Event) {
	e.
		Str("alias", i.alias).
		Str("host", i.host.PublishHost()).
		Str("debug_http_port", i.debugHTTPPort)
}

func (*runCommand) startRedisContainer(
	ctx context.Context,
	h contest.Host,
	whenExit func(container.WaitResponse, error),
) error {
	e := util.StringErrorFunc("failed to start container")

	port, err := h.FreePort("database-redis", "tcp")
	if err != nil {
		return e(err, "")
	}

	name := containerName("redis")

	if err := h.RemoveContainer(ctx, name, dockerTypes.ContainerRemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	}); err != nil {
		if !errors.Is(err, util.ErrNotFound) {
			return e(err, "")
		}
	}

	config := &container.Config{
		Hostname: name,
		Image:    DefaultRedisImage,
		Cmd: strslice.StrSlice{
			"redis-server",
			"--port",
			port,
		},
		Labels: map[string]string{"prog": contest.ContainerLabel},
	}

	hostconfig := &container.HostConfig{
		NetworkMode: container.NetworkMode("host"),
	}

	if err := h.CreateContainer(ctx, config, hostconfig, nil, name); err != nil {
		return e(err, "")
	}

	if err := h.StartContainer(ctx, hostconfig, nil, name, whenExit); err != nil {
		return e(err, "")
	}

	return nil
}

func (cmd *runCommand) initNode(
	ctx context.Context, host contest.Host, alias string, args []string,
) error {
	return cmd.doRunNode(ctx, host, alias, args)
}

func (cmd *runCommand) runNode(
	ctx context.Context, host contest.Host, alias string, args []string,
) error {
	port, err := host.FreePort(fmt.Sprintf("debug-http-port-%s", alias), "tcp")
	if err != nil {
		return err
	}

	args = append(args, //revive:disable-line:modifies-parameter
		fmt.Sprintf(`--dev.debug-http=:%s`, port), "--dev.pprof")

	if err := cmd.doRunNode(ctx, host, alias, args); err != nil {
		return err
	}

	_ = cmd.nodes.SetValue(alias, nodeInfo{alias: alias, debugHTTPPort: port, host: host})

	return nil
}

func (cmd *runCommand) doRunNode( //revive:disable-line:cyclomatic
	ctx context.Context, host contest.Host, alias string, args []string,
) error {
	e := util.StringErrorFunc("failed to run node")

	nargs := args

	var foundloglevel, foundlogformat, foundlogout bool

	for i := range nargs {
		switch {
		case strings.HasPrefix(nargs[i], "--log.level"):
			foundloglevel = true
		case strings.HasPrefix(nargs[i], "--log.format"):
			foundlogformat = true
		case strings.HasPrefix(nargs[i], "--log.out"):
			foundlogout = true
		}

		if foundloglevel && foundlogformat && foundlogout {
			break
		}
	}

	if !foundloglevel {
		nargs = append(nargs, "--log.level", "debug")
	}

	if !foundlogformat {
		nargs = append(nargs, "--log.format", "json")
	}

	if !foundlogout {
		nargs = append(nargs, "--log.out", "stdout")
	}

	name := containerName(alias)

	if err := host.RemoveContainer(ctx, name, dockerTypes.ContainerRemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	}); err != nil {
		if !errors.Is(err, util.ErrNotFound) {
			return e(err, "")
		}
	}

	config, hostconfig := cmd.nodeContainerConfigs(alias, host)
	config.Cmd = nargs

	if err := host.CreateContainer(ctx, config, hostconfig, nil, name); err != nil {
		return e(err, "")
	}

	lctx, logcancel := context.WithCancel(ctx)

	if err := host.StartContainer(
		ctx,
		hostconfig,
		nil,
		name,
		func(body container.WaitResponse, err error) {
			_ = cmd.nodes.RemoveValue(alias)

			logcancel()

			l := log.With().Stringer("logid", util.UUID()).Logger()

			func() *zerolog.Event {
				e := l.Debug()

				if err != nil && !errors.Is(err, context.Canceled) {
					e = l.Error().Err(err)
				}

				return e.Interface("body", body).
					Str("alias", alias).
					Str("container", name).
					Bool("ignore", cmd.design.IgnoreAbnormalContainerExit)
			}().Msg("container stopped")

			if !cmd.design.IgnoreAbnormalContainerExit && !errors.Is(err, context.Canceled) {
				var exiterr error

				switch {
				case err != nil:
					exiterr = err
				case body.StatusCode != 0:
					var errmsg string
					if body.Error != nil {
						errmsg = body.Error.Message + "; "
					}

					exiterr = errors.Errorf("%sexit=%d", errmsg, body.StatusCode)
				}

				if exiterr != nil {
					cmd.exitch <- exiterr

					return
				}
			}

			if err != nil {
				switch entry, eerr := contest.NewNodeLogEntryWithInterface(alias, true, bson.M{
					"container": name,
					"error":     err,
				}); {
				case eerr != nil:
					l.Error().Err(eerr).Msg("failed NodeLogEntry")
				default:
					cmd.logch <- entry
				}

				return
			}

			var bodyerr error

			if body.Error != nil {
				bodyerr = errors.Errorf(body.Error.Message)
			}

			entry, err := contest.NewNodeLogEntryWithInterface(alias, true, bson.M{
				"container": name,
				"error":     bodyerr,
				"exit_code": body.StatusCode,
			})
			if err != nil {
				l.Error().Err(err).Msg("failed NodeLogEntry")

				return
			}

			cmd.logch <- entry
		},
	); err != nil {
		return e(err, "")
	}

	if err := cmd.saveContainerLogs(lctx, alias); err != nil {
		return e(err, "")
	}

	return nil
}

func (cmd *runCommand) stopNodes(ctx context.Context, alias string, _ []string) error {
	e := util.StringErrorFunc("failed to stop node")

	name := containerName(alias)

	host := cmd.hosts.HostByContainer(name)
	if host == nil {
		return e(nil, "host not found")
	}

	switch _, info, found, err := host.ExistsContainer(ctx, name); {
	case err != nil:
		return e(err, "")
	case !found:
	case info == "running", info == "restarting":
		if err := host.StopContainer(ctx, name, nil); err != nil {
			return e(err, "")
		}
	}

	return nil
}

func (*runCommand) nodeContainerConfigs(alias string, host contest.Host) (
	*container.Config,
	*container.HostConfig,
) {
	name := containerName(alias)

	return &container.Config{
			Hostname:     name,
			User:         host.User(),
			Image:        DefaultNodeImage,
			AttachStdout: true,
			AttachStderr: true,
			WorkingDir:   "/data",
			Labels:       map[string]string{"prog": contest.ContainerLabel},
		},
		&container.HostConfig{
			NetworkMode: container.NetworkMode("host"),
			Mounts: []dockerMount.Mount{
				{
					Type:   dockerMount.TypeBind,
					Source: filepath.Join(host.Base(), "cmd"),
					Target: "/cmd",
				},
				{
					Type:   dockerMount.TypeBind,
					Source: filepath.Join(host.Base(), alias),
					Target: "/data",
				},
				{
					Type:   dockerMount.TypeBind,
					Source: filepath.Join(host.Base(), "genesis.yml"),
					Target: "/data/genesis.yml",
				},
				{
					Type:   dockerMount.TypeBind,
					Source: host.Base(),
					Target: "/host",
				},
			},
		}
}

func (cmd *runCommand) rangeNodes(
	ctx context.Context,
	action contest.ScenarioAction,
	f func(context.Context, contest.Host, string, []string) error,
) error {
	rv := action.RangeValues()
	if len(rv) < 1 {
		return errors.Errorf("empty range; `node` should be set in range")
	}

	for i := range rv {
		j, found := rv[i]["node"]
		if !found {
			return errors.Errorf("`node` not found in range; %q", rv[1])
		}

		alias := j.(string) //nolint:forcetypeassert //...

		host := cmd.hosts.HostByContainer(containerName(alias))
		if host == nil {
			return errors.Errorf("host not found; %q", alias)
		}

		vars := cmd.vars.Clone(nil)
		vars.Set(".self.host", host)
		vars.Set(".self.range", rv[i])

		args, err := action.CompileArgs(vars)
		if err != nil {
			return errors.WithMessage(err, alias)
		}

		if err := f(ctx, host, alias, args); err != nil {
			return errors.WithMessage(err, alias)
		}
	}

	return nil
}

func (cmd *runCommand) collectPprofs() {
	switch {
	case cmd.nodes.Len() < 1:
		return
	case cmd.PprofSeconds < 1:
		return
	}

	worker := util.NewDistributeWorker(context.Background(), int64(cmd.nodes.Len()), nil)
	defer worker.Close()

	cmd.nodes.Traverse(func(_ string, info nodeInfo) bool {
		if err := worker.NewJob(func(ctx context.Context, _ uint64) error {
			l := log.With().Object("node_info", info).Logger()

			l.Debug().Msg("trying to collect pprof")

			body, err := collectPprof(ctx, info, cmd.PprofSeconds)
			if err != nil {
				return nil
			}

			defer func() {
				_ = body.Close()
			}()

			fname := filepath.Join(cmd.basedir, fmt.Sprintf("trace-%s.out", info.alias))
			f, err := os.OpenFile(fname, os.O_WRONLY|os.O_CREATE, 0o600)
			if err != nil {
				l.Error().Err(err).Str("file", fname).Msg("failed to create trace file")

				return errors.WithStack(err)
			}

			if _, err := io.Copy(f, body); err != nil {
				l.Error().Err(err).Str("file", fname).Msg("failed to copy trace data")

				return errors.WithStack(err)
			}

			l.Debug().Msg("pprof collected")

			return nil
		}); err != nil {
			return false
		}

		return true
	})
	worker.Done()

	if err := worker.Wait(); err != nil {
		log.Error().Err(err).Msg("failed to collect")
	}
}

func containerName(alias string) string {
	return fmt.Sprintf("%s-%s", contest.ContainerLabel, alias)
}

func collectPprof(ctx context.Context, info nodeInfo, seconds uint) (io.ReadCloser, error) {
	u := fmt.Sprintf("http://%s/debug/pprof/trace?seconds=%d",
		net.JoinHostPort(info.host.PublishHost(), info.debugHTTPPort),
		seconds,
	)

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	req = req.WithContext(ctx)

	switch res, err := http.DefaultClient.Do(req); {
	case err != nil:
		return nil, errors.WithStack(err)
	case res.StatusCode != http.StatusOK:
		_ = res.Body.Close()

		return nil, errors.Errorf("not ok")
	default:
		return res.Body, nil
	}
}
