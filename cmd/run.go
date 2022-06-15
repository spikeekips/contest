package main

import (
	"bytes"
	"context"
	"debug/elf"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	dockerMount "github.com/docker/docker/api/types/mount"
	dockerClient "github.com/docker/docker/client"
	dockerstdcopy "github.com/docker/docker/pkg/stdcopy"
	"github.com/nxadm/tail"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	contest "github.com/spikeekips/contest2"
	"github.com/spikeekips/mitum/util"
	"github.com/spikeekips/mitum/util/localtime"
	"go.mongodb.org/mongo-driver/bson"
	"gopkg.in/yaml.v2"
)

var (
	contestID         = util.ULID().String()
	DefaultNodeImage  = "debian:testing-slim"
	DefaultRedisImage = "redis:latest"
)

var defaultMongodbURI = "mongodb://localhost:27017/contest_" + contestID

type runCommand struct {
	BaseDir      string        `arg:"" name:"base_directory" help:"base directory"`
	Scenario     string        `arg:"" name:"scenario" help:"scenario file" type:"existingfile"`
	Hosts        []HostFlag    `arg:"" name:"host" help:"docker host"`
	NodeBinaries []string      `name:"node-binary" help:"node binary files by architecture"`
	Mongodb      string        `name:"mongodb" help:"mongodb uri" default:"${mongodb_uri}"`
	Timeout      time.Duration `name:"timeout" help:"stop after timeout"`
	db           *contest.Mongodb
	basedir      string
	design       contest.Design
	vars         *contest.Vars
	hosts        *contest.Hosts
	logch        chan contest.LogEntry
	nodeBinaries map[elf.Machine]string
	exitch       chan error
}

func (cmd *runCommand) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cmd.prepare(); err != nil {
		return errors.Wrap(err, "")
	}

	cmd.exitch = make(chan error)

	if err := cmd.hosts.TraverseByHost(func(h contest.Host, _ []string) (bool, error) {
		if err := cmd.startRedisContainer(ctx, h, func(body container.ContainerWaitOKBody, err error) {
			if err != nil {
				cmd.exitch <- err

				return
			}

			if body.Error != nil {
				cmd.exitch <- errors.Errorf(body.Error.Message)
			}
		}); err != nil {
			return false, err
		}

		return true, nil
	}); err != nil {
		return errors.Wrap(err, "")
	}

	defer func() {
		err := cmd.closeHosts()
		if err != nil {
			log.Error().Err(err).Msg("failed to close hosts")
		}
	}()

	cmd.logch = make(chan contest.LogEntry)

	go cmd.saveLogs(ctx, cmd.logch)

	go func() {
		cmd.exitch <- cmd.watchLogs(ctx)
	}()

	cmd.logch <- contest.NewInternalLogEntry("contest ready", nil)

	select {
	case <-func() <-chan time.Time {
		if cmd.Timeout < 1 {
			return nil
		}

		return time.After(cmd.Timeout)
	}():
		cancel()

		log.Debug().Dur("timeout", cmd.Timeout).Msg("contest will be stopped by timeout")

		return errors.Errorf("timeout after %s", cmd.Timeout)
	case err := <-cmd.exitch:
		cancel()

		log.Debug().Err(err).Msg("contest will be stopped by exit chan")

		if err != nil {
			return err
		}
	}

	return nil
}

func (cmd *runCommand) closeHosts() error {
	log.Debug().Msg("trying to close hosts")
	defer log.Debug().Msg("hosts closed")

	switch {
	case cmd.hosts == nil:
		return nil
	case cmd.hosts.Len() == 1:
		if err := cmd.hosts.Close(); err != nil {
			return errors.Wrap(err, "")
		}
	}

	jobch := make(chan util.ContextWorkerCallback)

	go func() {
		_ = cmd.hosts.Traverse(func(host contest.Host) (bool, error) {
			jobch <- func(context.Context, uint64) error {
				_ = host.Close()

				return nil
			}

			return true, nil
		})

		close(jobch)
	}()

	if err := util.RunErrgroupWorkerByChan(context.Background(), int64(cmd.hosts.Len()), jobch); err != nil {
		return errors.Wrap(err, "")
	}

	return nil
}

func (cmd *runCommand) prepare() error {
	if err := cmd.prepareFlags(); err != nil {
		return errors.Wrap(err, "")
	}

	if err := cmd.prepareLogs(); err != nil {
		return errors.Wrap(err, "")
	}

	if err := cmd.prepareBase(); err != nil {
		return errors.Wrap(err, "")
	}

	if err := cmd.prepareHosts(); err != nil {
		return errors.Wrap(err, "")
	}

	if err := cmd.prepareScenario(); err != nil {
		return errors.Wrap(err, "")
	}

	log.Debug().Interface("vars", cmd.vars.Map()).Msg("vars")

	return nil
}

func (cmd *runCommand) prepareFlags() error {
	if len(cmd.NodeBinaries) < 1 {
		return errors.Errorf("empty node binaries")
	}

	cmd.nodeBinaries = map[elf.Machine]string{}
	for i := range cmd.NodeBinaries {
		p := cmd.NodeBinaries[i]

		switch f, err := elf.Open(p); {
		case err != nil:
			return errors.Wrap(err, "something wrong node binaries")
		case f.FileHeader.OSABI != elf.ELFOSABI_LINUX && f.FileHeader.OSABI != elf.ELFOSABI_NONE:
			return errors.Errorf("not supported os, %q", f.FileHeader.OSABI)
		default:
			if _, found := cmd.nodeBinaries[f.FileHeader.Machine]; found {
				return errors.Errorf("duplicated arch found, %s(%s)",
					p, contest.MachineToString(f.FileHeader.Machine))
			}

			cmd.nodeBinaries[f.FileHeader.Machine] = p
		}
	}

	if len(cmd.Hosts) < 1 {
		return errors.Errorf("empty host")
	}

	log.Debug().Str("id", contestID).Str("basedir", cmd.BaseDir).Func(func(e *zerolog.Event) {
		for i := range cmd.Hosts {
			e.Object("host", cmd.Hosts[i])
		}
	}).Msg("flags")

	return nil
}

func (cmd *runCommand) prepareHosts() error {
	e := util.StringErrorFunc("failed to prepare hosts")

	cmd.hosts = contest.NewHosts()

	for i := range cmd.Hosts {
		h := cmd.Hosts[i]

		var host contest.Host
		switch {
		case h.host == "localhost":
			i, err := contest.NewLocalHost(h.base, h.dockerhost)
			if err != nil {
				return e(err, "")
			}

			host = i
		default:
			i, err := contest.NewRemoteHost(filepath.Join(h.base, contestID), h.dockerhost)
			if err != nil {
				return e(err, "")
			}

			host = i
		}

		if err := cmd.hosts.New(host); err != nil {
			return e(err, "")
		}
	}

	if err := cmd.checkLocalPublishHost(); err != nil {
		return e(err, "")
	}

	jobch := make(chan util.ContextWorkerCallback)

	go func() {
		_ = cmd.hosts.Traverse(func(host contest.Host) (bool, error) {
			jobch <- func(ctx context.Context, _ uint64) error {
				if err := cmd.prepareBinaries(host); err != nil {
					return err
				}

				return nil
			}

			jobch <- func(ctx context.Context, _ uint64) error {
				if err := cmd.checkImages(host.Client(), DefaultNodeImage, DefaultRedisImage); err != nil {
					return err
				}

				return nil
			}

			jobch <- func(ctx context.Context, _ uint64) error {
				if _, err := host.FreePort("tcp"); err != nil {
					return err
				}

				if _, err := host.FreePort("udp"); err != nil {
					return err
				}

				return nil
			}

			return true, nil
		})

		close(jobch)
	}()

	if err := util.RunErrgroupWorkerByChan(context.Background(), int64(cmd.hosts.Len()), jobch); err != nil {
		return e(err, "")
	}

	return nil
}

func (cmd *runCommand) checkLocalPublishHost() error {
	var locals []contest.Host

	_ = cmd.hosts.Traverse(func(host contest.Host) (bool, error) {
		if host.Hostname() == "localhost" {
			locals = append(locals, host)
		}

		return true, nil
	})

	if len(locals) < 1 {
		return nil
	}

	var remoteside netip.Addr

	_ = cmd.hosts.Traverse(func(host contest.Host) (bool, error) {
		if host.Hostname() == "localhost" {
			return true, nil
		}

		addr, err := host.(*contest.RemoteHost).LocalAddr()
		if err != nil {
			return false, errors.Wrap(err, "")
		}

		switch {
		case !remoteside.IsValid():
			remoteside = addr
		case remoteside.IsLoopback(), remoteside.IsPrivate():
			remoteside = addr
		}

		return true, nil
	})

	if remoteside.IsValid() {
		for i := range locals {
			locals[i].(*contest.LocalHost).SetPublishHost(remoteside.String())
		}
	}

	return nil
}

func (cmd *runCommand) prepareBinaries(host contest.Host) error {
	j, found := cmd.nodeBinaries[host.Arch()]
	if !found {
		return errors.Errorf("node binary does not support target host arch, %q",
			contest.MachineToString(host.Arch()))
	}

	f, _ := os.Open(j)

	defer func() {
		_ = f.Close()
	}()

	if err := host.Upload(f, "node-binary", 0o700); err != nil {
		return errors.Wrap(err, "failed to upload node binary")
	}

	return nil
}

func (cmd *runCommand) prepareBase() error {
	e := util.StringErrorFunc("failed to prepare base directory")

	scenario := filepath.Base(cmd.Scenario)
	scenario = scenario[:len(scenario)-len(filepath.Ext(scenario))]

	base := filepath.Join(
		cmd.BaseDir,
		fmt.Sprintf("%s-%s-%s", contestID, localtime.Now().Format("20060102T150405.999999999"), scenario),
	)

	switch fi, err := os.Stat(base); {
	case err == nil:
		if !fi.IsDir() {
			return e(nil, "base directory,%q not directory", base)
		}

		if err := os.RemoveAll(base); err != nil {
			return e(err, "")
		}
	case !os.IsNotExist(err):
		return e(err, "")
	}

	if err := os.MkdirAll(base, 0o700); err != nil {
		return e(err, "")
	}

	switch abs, err := filepath.Abs(base); {
	case err != nil:
		return e(err, "")
	default:
		cmd.basedir = abs
	}

	for i := range cmd.Hosts {
		h := cmd.Hosts[i]

		if h.host == "localhost" && len(h.base) < 1 {
			h.base = cmd.basedir
			cmd.Hosts[i] = h
		}
	}

	return nil
}

func (cmd *runCommand) prepareLogs() error {
	db, err := contest.NewMongodbFromURI(context.Background(), cmd.Mongodb)
	if err != nil {
		return err
	}

	cmd.db = db

	return nil
}

func (cmd *runCommand) prepareScenario() error {
	e := util.StringErrorFunc("failed to load scenario")

	i, err := os.ReadFile(cmd.Scenario)
	if err != nil {
		return e(err, "")
	}

	log.Debug().Str("content", string(i)).Msg("scenario")

	if err := yaml.Unmarshal([]byte(i), &cmd.design); err != nil {
		return e(err, "")
	}

	if err := cmd.design.IsValid(nil); err != nil {
		return e(err, "")
	}

	log.Debug().Interface("scenario", cmd.design).Msg("scenario loaded")

	vars := contest.NewVars(nil)

	// NOTE global vars
	for k := range cmd.design.Vars {
		vars.Set(k, cmd.design.Vars[k])
	}

	vars = vars.AddFunc("nodePublishAddr", func(host contest.Host, id, network, innerport string) string {
		if host == nil {
			return "<no value>"
		}

		port, err := host.ContainerFreePort(containerName(id), network, innerport)
		if err != nil {
			return "<no value>"
		}

		return host.PublishHost() + ":" + port
	})

	// NOTE nodes design
	designs := map[string]string{}

	if _, err := cmd.hosts.NewContainer(containerName("redis")); err != nil {
		return e(err, "")
	}

	nodes := cmd.design.NodeDesigns.AllNodes()

	for i := range nodes {
		alias := nodes[i]

		host, err := cmd.hosts.NewContainer(containerName(alias))
		if err != nil {
			return e(err, "")
		}

		extra := map[string]interface{}{
			"self": map[string]interface{}{
				"alias": alias,
				"host":  host,
			},
		}

		bc, err := contest.CompileTemplate(cmd.design.NodeDesigns.Common, vars, extra)
		if err != nil {
			return e(err, "failed to compile common design for %s", alias)
		}

		bn, err := contest.CompileTemplate(cmd.design.NodeDesigns.Nodes[alias], vars, extra)
		if err != nil {
			return e(err, "failed to compile node design for %s", alias)
		}

		designs[alias] = strings.TrimSpace(bc+"\n"+bn) + "\n"

		vars.Rename(".self", ".nodes."+alias)

		log.Debug().Str("node", alias).Interface("design", designs[alias]).Msg("node design generated")
	}

	genesis, err := contest.CompileTemplate(cmd.design.NodeDesigns.Genesis, vars, nil)
	if err != nil {
		return e(err, "failed to compile genesis design")
	}

	if err := cmd.hosts.TraverseByHost(func(h contest.Host, _ []string) (bool, error) {
		if err := h.Upload(
			bytes.NewBuffer([]byte(genesis)),
			"genesis.yml",
			0o600,
		); err != nil {
			return false, e(err, "")
		}

		return true, nil
	}); err != nil {
		return e(err, "failed to upload genesis.yml")
	}

	for alias := range designs {
		host := cmd.hosts.HostByContainer(containerName(alias))
		if host == nil {
			return e(nil, "not found in host")
		}

		configfile := filepath.Join(cmd.basedir, alias+".yml")
		if err := func() error {
			f, err := os.OpenFile(configfile, os.O_WRONLY|os.O_CREATE, 0o600)
			if err != nil {
				return errors.Wrapf(err, "failed to create node design file for %q", alias)
			}

			defer func() {
				_ = f.Close()
			}()

			if _, err := f.WriteString(designs[alias]); err != nil {
				return errors.Wrapf(err, "failed to write node design file for %q", alias)
			}

			return nil
		}(); err != nil {
			return e(err, "")
		}

		if err := host.Mkdir(
			alias,
			0o700,
		); err != nil {
			return e(err, "")
		}

		if err := host.Upload(
			bytes.NewBuffer([]byte(designs[alias])),
			filepath.Join(alias, "config.yml"),
			0o600,
		); err != nil {
			return e(err, "")
		}

	}

	cmd.vars = vars

	return nil
}

func (cmd *runCommand) watchLogs(ctx context.Context) error {
	expects := make([]contest.ExpectScenario, len(cmd.design.Expects))
	copy(expects, cmd.design.Expects)

	var active contest.ExpectScenario

	newactive := func() error {
		active, expects = expects[0], expects[1:]

		i, err := active.Compile(cmd.vars)
		if err != nil {
			return errors.Wrap(err, "")
		}

		log.Debug().Interface("expect", i).Msg("new expect")

		return nil
	}

	if err := newactive(); err != nil {
		return errors.Wrap(err, "")
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

end:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			switch ok, err := cmd.evaluateLog(ctx, active); {
			case err != nil:
				return errors.Wrap(err, "")
			case ok:
				log.Debug().Interface("expect", active).Msg("matched")

				if len(expects) < 1 { // NOTE finished
					break end
				}

				if err := newactive(); err != nil {
					return errors.Wrap(err, "")
				}
			}
		}
	}

	log.Info().Msg("finished")

	return nil
}

func (cmd *runCommand) evaluateLog(ctx context.Context, expect contest.ExpectScenario) (bool, error) {
	var ok bool

	current, err := expect.Compile(cmd.vars)
	if err != nil {
		return false, errors.Wrap(err, "")
	}

	var query bson.M
	if err := bson.UnmarshalExtJSON([]byte(current.Condition), false, &query); err != nil {
		return false, errors.Wrap(err, "")
	}

	r, found, err := cmd.db.Find(ctx, query)
	switch {
	case err != nil:
		return false, errors.Wrap(err, "")
	case !found:
		return false, nil
	default:
		ok = found
	}

	for i := range current.Actions {
		action := current.Actions[i]

		l := log.With().Stringer("logid", util.UUID()).Logger()
		l.Debug().Interface("action", action).Msg("trying to run action")

		if err := cmd.action(ctx, action); err != nil {
			l.Error().Err(err).Msg("failed to run action")

			return ok, errors.Wrap(err, "")
		}

		l.Debug().Msg("action done")
	}

	for i := range current.Registers {
		register := current.Registers[i]

		l := log.With().Stringer("registerid", util.UUID()).Logger()
		l.Debug().Interface("register", register).Msg("trying to register")

		if err := cmd.register(r, register); err != nil {
			l.Error().Err(err).Msg("failed to register")

			return ok, errors.Wrap(err, "")
		}

		l.Debug().Msg("register done")
	}

	return true, nil
}

func (cmd *runCommand) register(
	record map[string]interface{}, register contest.ScenarioRegister,
) error {
	cmd.vars.Set(register.Assign, record)

	return nil
}

func (cmd *runCommand) action(ctx context.Context, action contest.ScenarioAction) error {
	switch action.Type {
	case "init-nodes":
		if len(action.Args) < 1 {
			return errors.Errorf("empty nodes")
		}

		for i := range action.Args {
			alias := action.Args[i]

			if err := cmd.initNode(ctx, alias); err != nil {
				return errors.Wrapf(err, "failed to init node, %q", alias)
			}
		}
	case "start-nodes":
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
		if len(action.Args) < 1 {
			return errors.Errorf("empty nodes")
		}
	}

	return nil
}

func (cmd *runCommand) saveLogs(ctx context.Context, ch chan contest.LogEntry) {
	var entries []contest.LogEntry

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	dbsavelog := func() error {
		if len(entries) < 1 {
			return nil
		}

		if err := cmd.db.InsertLogEntries(ctx, entries); err != nil {
			return err
		}

		entries = nil

		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return
		case e, notclosed := <-ch:
			if !notclosed {
				if err := dbsavelog(); err != nil {
					log.Error().Err(err).Msg("failed to save logs")
				}

				return
			}

			entries = append(entries, e)
		case <-ticker.C:
			if err := dbsavelog(); err != nil {
				log.Error().Err(err).Msg("failed to save logs")
			}
		}
	}
}

func (cmd *runCommand) checkImages(client *dockerClient.Client, images ...string) error {
	for i := range images {
		image := images[i]

		switch found, err := contest.ExistsImage(client, image); {
		case err != nil:
			return errors.Wrapf(err, "failed to check image, %q", image)
		case !found:
			if err := contest.PullImage(client, image); err != nil {
				return errors.Wrapf(err, "failed to pull image, %q", image)
			}
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
	}

	if err := h.CreateContainer(ctx, config, nil, nil, name); err != nil {
		return e(err, "")
	}

	if err := h.StartContainer(ctx, config, nil, nil, name, whenExit); err != nil {
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
	config.Cmd = []string{"/node-binary", "init", "config.yml", "genesis.yml"}
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

func (cmd *runCommand) saveContainerLogs(ctx context.Context, alias string) error {
	name := containerName(alias)

	host := cmd.hosts.HostByContainer(name)
	if host == nil {
		return errors.Errorf("host not found")
	}

	openfiles := func(fname string) (io.WriteCloser, *tail.Tail, error) {
		f, err := os.OpenFile(fname, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, err
		}

		t, err := tail.TailFile(fname, tail.Config{Follow: true})
		if err != nil {
			return nil, nil, err
		}

		return f, t, nil
	}

	logstdoutfilename := filepath.Join(cmd.basedir, alias+".stdout.log")
	logstderrfilename := filepath.Join(cmd.basedir, alias+".stderr.log")

	outf, outt, err := openfiles(logstdoutfilename)
	if err != nil {
		return err
	}

	errf, errt, err := openfiles(logstderrfilename)
	if err != nil {
		return err
	}

	r, err := host.ContainerLogs(ctx, name, dockerTypes.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true,
		Follow: true, Tail: "all",
	})
	if err != nil {
		return err
	}

	go func() {
		defer func() {
			_ = r.Close()
			_ = outf.Close()
			_ = errf.Close()
		}()

		if _, err := dockerstdcopy.StdCopy(outf, errf, r); err != nil {
			log.Debug().Err(err).Str("container", name).Msg("saving container logs stopped")
		}
	}()

	savetail := func(t *tail.Tail, stderr bool) {
		for l := range t.Lines {
			if ctx.Err() != nil {
				break
			}

			if l.Err != nil {
				cmd.logch <- contest.NewInternalLogEntry("tail error", l.Err)
			}

			if len(l.Text) > 0 {
				if e := log.Trace(); e.Enabled() {
					e.Str("node", alias).Str("text", l.Text).Msg("new log text")
				}

				switch entry, err := contest.NewNodeLogEntry(alias, stderr, []byte(l.Text)); {
				case err != nil:
					log.Error().Err(err).Str("node", alias).Str("text", l.Text).Msg("wrong node log")
				default:
					cmd.logch <- entry
				}
			}
		}
	}

	go savetail(outt, false)
	go savetail(errt, true)

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
			Cmd:          []string{"/node-binary", "init", "config.yml", "genesis.yml"},
		},
		&container.HostConfig{
			Links: []string{containerName("redis") + ":redis"},
			Mounts: []dockerMount.Mount{
				{
					Type:   dockerMount.TypeBind,
					Source: filepath.Join(host.Base(), "node-binary"),
					Target: "/node-binary",
				},
				{
					Type:   dockerMount.TypeBind,
					Source: filepath.Join(host.Base(), alias),
					Target: "/data",
				},
			},
		}
}

func containerName(alias string) string {
	return fmt.Sprintf("%s-%s", contest.ContainerLabel, alias)
}
