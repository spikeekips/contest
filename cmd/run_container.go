package main

import (
	"context"
	"fmt"
	"path/filepath"

	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	dockerMount "github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/strslice"
	"github.com/pkg/errors"
	contest "github.com/spikeekips/contest2"
	"github.com/spikeekips/mitum/util"
	"go.mongodb.org/mongo-driver/bson"
)

func (cmd *runCommand) action(ctx context.Context, action contest.ScenarioAction) error {
	switch action.Type {
	case "run-nodes":
		if err := cmd.rangeNodes(ctx, action, cmd.runNode); err != nil {
			return errors.Wrap(err, "failed to run node")
		}
	case "init-nodes": // FIXME remove
		if len(action.Args) < 1 {
			return errors.Errorf("empty nodes")
		}

		for i := range action.Args {
			alias := action.Args[i]

			if err := cmd.initNode(ctx, alias); err != nil {
				return errors.Wrapf(err, "failed to init node, %q", alias)
			}
		}
	case "start-nodes": // FIXME remove
		if len(action.Args) < 1 {
			return errors.Errorf("empty nodes")
		}

		for i := range action.Args {
			alias := action.Args[i]

			if err := cmd.startNode(ctx, alias); err != nil {
				return errors.Wrapf(err, "failed to start node, %q", alias)
			}
		}
	case "stop-nodes":
		if err := cmd.rangeNodes(ctx, action, func(ctx context.Context, alias string, _ []string) error {
			_ = cmd.stopNodes(ctx, alias, nil)

			// NOTE ignore error

			return nil
		}); err != nil {
			return errors.Wrap(err, "failed to stop node")
		}
	}

	return nil
}

func (cmd *runCommand) startRedisContainer(
	ctx context.Context,
	h contest.Host,
	whenExit func(container.ContainerWaitOKBody, error),
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
	}

	hostconfig := &container.HostConfig{
		NetworkMode: container.NetworkMode("host"),
	}

	if err := h.CreateContainer(ctx, config, hostconfig, nil, name); err != nil {
		return e(err, "")
	}

	if err := h.StartContainer(ctx, config, hostconfig, nil, name, whenExit); err != nil {
		return e(err, "")
	}

	return nil
}

func (cmd *runCommand) initNode(ctx context.Context, alias string) error {
	e := util.StringErrorFunc("failed to init node")

	name := containerName(alias)

	host := cmd.hosts.HostByContainer(name)
	if host == nil {
		return e(nil, "host not found")
	}

	if err := host.RemoveContainer(ctx, name, dockerTypes.ContainerRemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	}); err != nil {
		if !errors.Is(err, util.ErrNotFound) {
			return e(err, "")
		}
	}

	config, hostconfig := cmd.nodeContainerConfigs(alias, host)
	config.Cmd = []string{"/cmd", "init", "config.yml", "genesis.yml"}
	hostconfig.Mounts = append(hostconfig.Mounts, dockerMount.Mount{
		Type:   dockerMount.TypeBind,
		Source: filepath.Join(host.Base(), "genesis.yml"),
		Target: "/data/genesis.yml",
	})

	if err := host.CreateContainer(ctx, config, hostconfig, nil, name); err != nil {
		return e(err, "")
	}

	if err := host.StartContainer(
		ctx,
		config,
		hostconfig,
		nil,
		name,
		func(body container.ContainerWaitOKBody, err error) {
			if ctx.Err() != nil && errors.Is(err, context.Canceled) {
				return
			}

			l := log.With().Stringer("logid", util.UUID()).Logger()

			l.Err(err).Interface("body", body).
				Str("alias", alias).
				Str("container", name).
				Msg("container stopped")

			if !cmd.design.IgnoreWhenAbnormalContainerExit {
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
				entry, err := contest.NewNodeLogEntryWithInterface(alias, true, bson.M{
					"container": name,
					"error":     err,
				})
				if err != nil {
					l.Error().Err(err).Msg("failed NodeLogEntry")

					return
				}

				cmd.logch <- entry

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

	if err := cmd.saveContainerLogs(ctx, alias); err != nil {
		return e(err, "")
	}

	return nil
}

func (cmd *runCommand) startNode(ctx context.Context, alias string) error {
	e := util.StringErrorFunc("failed to start node")

	name := containerName(alias)

	host := cmd.hosts.HostByContainer(name)
	if host == nil {
		return e(nil, "host not found")
	}

	if err := host.RemoveContainer(ctx, name, dockerTypes.ContainerRemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	}); err != nil {
		if !errors.Is(err, util.ErrNotFound) {
			return e(err, "")
		}
	}

	config, hostconfig := cmd.nodeContainerConfigs(alias, host)
	config.Cmd = []string{"bash", "-c", "n=0; while :; do [ $n -gt 10 ] && exit 1; date; pwd; find /data; sleep 1; n=$(expr $n + 1); done"} // FIXME

	if err := host.CreateContainer(ctx, config, hostconfig, nil, name); err != nil {
		return e(err, "")
	}

	if err := host.StartContainer(
		ctx,
		config,
		hostconfig,
		nil,
		name,
		func(body container.ContainerWaitOKBody, err error) {
			l := log.With().Stringer("logid", util.UUID()).Logger()

			l.Err(err).Interface("body", body).
				Str("alias", alias).
				Str("container", name).
				Msg("container stopped")

			if err != nil {
				entry, err := contest.NewNodeLogEntryWithInterface(alias, true, bson.M{
					"container": name,
					"error":     err,
				})
				if err != nil {
					l.Error().Err(err).Msg("failed NodeLogEntry")

					return
				}

				cmd.logch <- entry

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

	if err := cmd.saveContainerLogs(ctx, alias); err != nil {
		return e(err, "")
	}

	return nil
}

func (cmd *runCommand) runNode(ctx context.Context, alias string, args []string) error {
	e := util.StringErrorFunc("failed to run node")

	name := containerName(alias)

	host := cmd.hosts.HostByContainer(name)
	if host == nil {
		return e(nil, "host not found")
	}

	if err := host.RemoveContainer(ctx, name, dockerTypes.ContainerRemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	}); err != nil {
		if !errors.Is(err, util.ErrNotFound) {
			return e(err, "")
		}
	}

	config, hostconfig := cmd.nodeContainerConfigs(alias, host)
	config.Cmd = args

	if err := host.CreateContainer(ctx, config, hostconfig, nil, name); err != nil {
		return e(err, "")
	}

	if err := host.StartContainer(
		ctx,
		config,
		hostconfig,
		nil,
		name,
		func(body container.ContainerWaitOKBody, err error) {
			l := log.With().Stringer("logid", util.UUID()).Logger()

			l.Err(err).Interface("body", body).
				Str("alias", alias).
				Str("container", name).
				Msg("container stopped")

			if err != nil {
				entry, err := contest.NewNodeLogEntryWithInterface(alias, true, bson.M{
					"container": name,
					"error":     err,
				})
				if err != nil {
					l.Error().Err(err).Msg("failed NodeLogEntry")

					return
				}

				cmd.logch <- entry

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

	if err := cmd.saveContainerLogs(ctx, alias); err != nil {
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

	if err := host.StopContainer(ctx, name, nil); err != nil {
		if !errors.Is(err, util.ErrNotFound) {
			return e(err, "")
		}
	}

	return nil
}

func (cmd *runCommand) nodeContainerConfigs(alias string, host contest.Host) (
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
	f func(context.Context, string, []string) error,
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

		alias := j.(string)

		host := cmd.hosts.HostByContainer(containerName(alias))
		if host == nil {
			return errors.Errorf("host not found; %q", alias)
		}

		vars := cmd.vars.Clone(nil)
		vars.Set(".self.host", host)
		vars.Set(".self.range", rv[i])

		args, err := action.CompileArgs(vars)
		if err != nil {
			return errors.Wrap(err, alias)
		}

		if err := f(ctx, alias, args); err != nil {
			return errors.Wrap(err, alias)
		}
	}

	return nil
}

func containerName(alias string) string {
	return fmt.Sprintf("%s-%s", contest.ContainerLabel, alias)
}
