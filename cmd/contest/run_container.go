package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types/container"
	dockerMount "github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/strslice"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/spikeekips/contest"
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
	e := util.StringError("start container")

	port, err := h.FreePort("database-redis", "tcp")
	if err != nil {
		return e.Wrap(err)
	}

	name := containerName("redis")

	if err := h.RemoveContainer(ctx, name, container.RemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	}); err != nil {
		if !errors.Is(err, util.ErrNotFound) {
			return e.Wrap(err)
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
		return e.Wrap(err)
	}

	if err := h.StartContainer(ctx, hostconfig, nil, name, whenExit); err != nil {
		return e.Wrap(err)
	}

	return nil
}

var nginxConf = `
error_log  /dev/stderr;
events {}

http {
  server {
    access_log /dev/stdout;
    listen @@port@@;
    sendfile on;
    tcp_nodelay on;
	autoindex on;
  }
}
`

func (*runCommand) startNginxContainer(
	ctx context.Context,
	h contest.Host,
	properties map[string]interface{},
	whenExit func(container.WaitResponse, error),
) error {
	e := util.StringError("start nginx container")

	var id string

	switch found, err := contest.ScenarioActionProperty(properties, "name", &id); {
	case err != nil:
		return err //nolint:wrapcheck //...
	case !found:
		return errors.Errorf("name not found")
	}

	var root string

	switch found, err := contest.ScenarioActionProperty(properties, "root", &root); {
	case err != nil:
		return err //nolint:wrapcheck //...
	case !found:
		return errors.Errorf("root not found")
	}

	var port string

	switch found, err := contest.ScenarioActionProperty(properties, "port", &port); {
	case err != nil:
		return err //nolint:wrapcheck //...
	case !found:
		port = strings.TrimSpace(port)

		return errors.Errorf("port not found")
	}

	cname := containerName(id)

	if err := h.RemoveContainer(ctx, cname, container.RemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	}); err != nil {
		if !errors.Is(err, util.ErrNotFound) {
			return e.Wrap(err)
		}
	}

	confb := bytes.NewBuffer([]byte(strings.ReplaceAll(nginxConf, "@@port@@", port)))
	confname := id + "-nginx.conf"

	var hostconfname string

	switch err := h.Upload(confb, confname, confname, 0o600); {
	case err != nil:
		return err //nolint:wrapcheck //...
	default:
		i, found := h.File(confname)
		if !found {
			return errors.Errorf("nginx conf not found")
		}

		hostconfname = i
	}

	config := &container.Config{
		Hostname: id,
		Image:    DefaultNginxImage,
		Labels:   map[string]string{"prog": contest.ContainerLabel},
	}

	hostconfig := &container.HostConfig{
		NetworkMode: container.NetworkMode("host"),
		Mounts: []dockerMount.Mount{
			{
				Type:   dockerMount.TypeBind,
				Source: root,
				Target: "/etc/nginx/html",
			},
			{
				Type:   dockerMount.TypeBind,
				Source: hostconfname,
				Target: "/etc/nginx/nginx.conf",
			},
		},
	}

	if err := h.CreateContainer(ctx, config, hostconfig, nil, cname); err != nil {
		return e.Wrap(err)
	}

	if err := h.StartContainer(ctx, hostconfig, nil, cname, whenExit); err != nil {
		return e.Wrap(err)
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
		return err //nolint:wrapcheck //...
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
	e := util.StringError("run node")

	var fargs []string
	options := []string{
		"--log.level", "debug",
		"--log.format", "json",
		"--log.out", "stdout",
		"--log.out", "/host/" + alias + "-stdout-log.json",
	}

	var foundoption bool

	for i := range args {
		j := args[i]

		if !foundoption && strings.HasPrefix(j, "--") {
			foundoption = true
		}

		switch {
		case foundoption:
			options = append(options, j)
		default:
			fargs = append(fargs, j)
		}
	}

	nargs := make([]string, len(fargs)+len(options))
	copy(nargs, fargs)
	copy(nargs[len(fargs):], options)

	name := containerName(alias)

	if err := host.RemoveContainer(ctx, name, container.RemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	}); err != nil {
		if !errors.Is(err, util.ErrNotFound) {
			return e.Wrap(err)
		}
	}

	config, hostconfig := cmd.nodeContainerConfigs(alias, host)
	config.Cmd = nargs

	if err := host.CreateContainer(ctx, config, hostconfig, nil, name); err != nil {
		return e.Wrap(err)
	}

	if err := host.StartContainer(
		ctx,
		hostconfig,
		nil,
		name,
		func(body container.WaitResponse, err error) {
			_ = cmd.nodes.RemoveValue(alias)

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
				bodyerr = errors.New(body.Error.Message)
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
		return e.Wrap(err)
	}

	if err := cmd.saveContainerLogs(ctx, alias); err != nil {
		return e.Wrap(err)
	}

	return nil
}

func (cmd *runCommand) stopNodes(ctx context.Context, alias string, _ []string) error {
	e := util.StringError("stop node")

	name := containerName(alias)

	host := cmd.hosts.HostByContainer(name)
	if host == nil {
		return e.Errorf("host not found")
	}

	switch _, info, found, err := host.ExistsContainer(ctx, name); {
	case err != nil:
		return e.Wrap(err)
	case !found:
	case info == "running", info == "restarting":
		if err := host.StopContainer(ctx, name, nil); err != nil {
			return e.Wrap(err)
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
	f func(context.Context, contest.Host, string, []string, map[string]interface{}) error,
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

		switch i, found := vars.Value(".nodes." + alias); {
		case !found:
			return errors.Errorf("node vars not found")
		default:
			vars.Set(".self", i)
		}

		vars.Set(".self.host", host)
		vars.Set(".self.range", rv[i])

		args, err := action.CompileArgs(vars)
		if err != nil {
			return errors.WithMessage(err, alias)
		}

		properties, err := action.CompileProperties(vars)
		if err != nil {
			return errors.WithMessage(err, alias)
		}

		if err := f(ctx, host, alias, args, properties); err != nil {
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

	worker, _ := util.NewErrCallbackJobWorker(context.Background(), int64(cmd.nodes.Len()), nil)
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

	req, err := http.NewRequest(http.MethodGet, u, http.NoBody)
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
