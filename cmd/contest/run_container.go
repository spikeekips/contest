package main

import (
	"context"
	"fmt"
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

func (cmd *runCommand) runNode( //revive:disable-line:cyclomatic
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

func containerName(alias string) string {
	return fmt.Sprintf("%s-%s", contest.ContainerLabel, alias)
}
