package main

import (
	"context"
	"debug/elf"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	dockerClient "github.com/docker/docker/client"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	contest "github.com/spikeekips/contest2"
	"github.com/spikeekips/mitum/util"
	"github.com/spikeekips/mitum/util/logging"
	"gopkg.in/yaml.v3"
)

var DefaultHostBase = "/tmp/contest"

func (cmd *runCommand) prepare() error {
	if err := cmd.prepareFlags(); err != nil {
		return err
	}

	if err := cmd.prepareLogs(); err != nil {
		return err
	}

	if err := cmd.prepareBase(); err != nil {
		return err
	}

	if err := cmd.prepareDesign(); err != nil {
		return err
	}

	if err := cmd.prepareHosts(); err != nil {
		return err
	}

	if err := cmd.prepareScenario(); err != nil {
		return err
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

	log.Debug().
		Str("id", contestID).
		Str("basedir", cmd.BaseDir).
		Func(func(e *zerolog.Event) {
			for i := range cmd.Hosts {
				e.Object("host", cmd.Hosts[i])
			}
		}).
		Str("design", cmd.Design).
		Interface("hosts", cmd.Hosts).
		Strs("node_binaries", cmd.NodeBinaries).
		Str("mongodb", cmd.Mongodb).
		Dur("timeout", cmd.Timeout).
		Uint("pprof_seconds", cmd.PprofSeconds).
		Msg("flags")

	return nil
}

func (cmd *runCommand) prepareHosts() error {
	e := util.StringError("prepare hosts")

	samehost := make([]string, len(cmd.design.Nodes.SameHost))
	for i := range cmd.design.Nodes.SameHost {
		samehost[i] = containerName(cmd.design.Nodes.SameHost[i])
	}

	cmd.hosts = contest.NewHosts(samehost)

	for i := range cmd.Hosts {
		h := cmd.Hosts[i]

		var host contest.Host

		switch {
		case h.host == "localhost":
			i, err := contest.NewLocalHost(h.base, h.dockerhost)
			if err != nil {
				return e.Wrap(err)
			}

			host = i
		default:
			i, err := contest.NewRemoteHost(h.base, h.dockerhost)
			if err != nil {
				return e.Wrap(err)
			}

			host = i
		}

		if l, ok := host.(logging.SetLogging); ok {
			_ = l.SetLogging(mlogging)
		}

		if err := cmd.hosts.New(host); err != nil {
			return e.Wrap(err)
		}
	}

	if err := cmd.checkLocalPublishHost(); err != nil {
		return e.Wrap(err)
	}

	worker, _ := util.NewErrgroupWorker(context.Background(), int64(cmd.hosts.Len()))
	defer worker.Close()

	_ = cmd.hosts.Traverse(func(host contest.Host) (bool, error) {
		if err := worker.NewJob(func(context.Context, uint64) error {
			if err := cmd.prepareBinaries(host); err != nil {
				return err
			}

			if _, err := host.FreePort("check", "tcp"); err != nil {
				return errors.WithStack(err)
			}

			if _, err := host.FreePort("check", "udp"); err != nil {
				return errors.WithStack(err)
			}

			return nil
		}); err != nil {
			return false, err
		}

		if err := worker.NewJob(func(context.Context, uint64) error {
			return cmd.checkImages(host.Client(), DefaultNodeImage, DefaultRedisImage)
		}); err != nil {
			return false, err
		}

		return true, nil
	})

	worker.Done()

	return worker.Wait()
}

func (cmd *runCommand) prepareBinaries(host contest.Host) error {
	i, found := cmd.nodeBinaries[host.Arch()]
	if !found {
		return errors.Errorf("node binary does not support target host arch, %q",
			contest.MachineToString(host.Arch()))
	}

	f, err := os.Open(i)
	if err != nil {
		return errors.WithStack(err)
	}

	defer func() {
		_ = f.Close()
	}()

	if err := host.Upload(f, "cmd", "cmd", 0o700); err != nil {
		return errors.WithMessage(err, "upload node binary")
	}

	return nil
}

func (cmd *runCommand) prepareBase() error {
	e := util.StringError("prepare base directory")

	switch fi, err := os.Stat(cmd.BaseDir); {
	case err == nil:
		if !fi.IsDir() {
			return e.Errorf("base directory,%q not directory", cmd.BaseDir)
		}
	case !os.IsNotExist(err):
		return e.Wrap(err)
	default:
		if err := os.MkdirAll(cmd.BaseDir, 0o700); err != nil {
			return e.Wrap(err)
		}
	}

	var abs string

	switch i, err := filepath.Abs(cmd.BaseDir); {
	case err != nil:
		return e.Wrap(err)
	default:
		cmd.basedir = i

		abs = i
	}

	for i := range cmd.Hosts {
		h := cmd.Hosts[i]

		switch {
		case h.host == "localhost":
			if len(h.base) < 1 {
				h.base = abs
			}
		default:
			if len(h.base) < 1 {
				h.base = DefaultHostBase
			}
		}

		cmd.Hosts[i] = h
	}

	return nil
}

func (cmd *runCommand) prepareLogs() error {
	db, err := contest.NewMongodbFromURI(context.Background(), cmd.Mongodb)
	if err != nil {
		return errors.WithStack(err)
	}

	cmd.db = db

	return nil
}

func (cmd *runCommand) prepareDesign() error {
	e := util.StringError("load design")

	i, err := os.ReadFile(cmd.Design)
	if err != nil {
		return e.Wrap(err)
	}

	log.Debug().Str("content", string(i)).Msg("design")

	if err := yaml.Unmarshal(i, &cmd.design); err != nil {
		return e.Wrap(err)
	}

	if err := cmd.design.IsValid(nil); err != nil {
		return e.Wrap(err)
	}

	return nil
}

func (cmd *runCommand) prepareScenario() error { //revive:disable-line:cognitive-complexity,function-length,cyclomatic
	e := util.StringError("load scenario")

	log.Debug().Interface("scenario", cmd.design).Msg("scenario loaded")

	vars := contest.NewVars(nil)

	// NOTE global vars
	for k := range cmd.design.Vars {
		vars.Set(k, cmd.design.Vars[k])
	}

	vars = vars.AddFunc("uuid", func() string {
		return util.UUID().String()
	})

	vars = vars.AddFunc("ulid", func() string {
		return util.ULID().String()
	})

	vars = vars.AddFunc("hostFile", func(host contest.Host, f string) string {
		if host == nil {
			return "<no value>"
		}

		path, found := host.File(f)
		if !found {
			return "<no value>"
		}

		return path
	})

	vars = vars.AddFunc("hostBase", func(host contest.Host) string {
		if host == nil {
			return "<no value>"
		}

		return host.Base()
	})

	vars = vars.AddFunc("freePort", func(host contest.Host, id, network string) string {
		if host == nil {
			return "<no value>"
		}

		port, err := host.FreePort(id, network)
		if err != nil {
			return "<no value>"
		}

		return port
	})

	// NOTE nodes design
	designs := map[string]string{}

	nodes := cmd.design.Designs.AllNodes()

	for i := range nodes {
		alias := nodes[i]

		host, err := cmd.hosts.NewContainer(containerName(alias))
		if err != nil {
			return e.Wrap(err)
		}

		extra := map[string]interface{}{
			"self": map[string]interface{}{
				"alias": alias,
				"host":  host,
			},
		}

		bc, err := contest.CompileTemplate(cmd.design.Designs.Common, vars, extra)
		if err != nil {
			return e.WithMessage(err, "compile common design for %s", alias)
		}

		designs[alias] = strings.TrimSpace(bc) + "\n"

		vars.Rename(".self", ".nodes."+alias)
	}

	for i := range nodes {
		alias := nodes[i]

		host, err := cmd.hosts.NewContainer(containerName(alias))
		if err != nil {
			return e.Wrap(err)
		}

		extra := map[string]interface{}{
			"self": map[string]interface{}{
				"alias": alias,
				"host":  host,
			},
		}

		bn, err := contest.CompileTemplate(cmd.design.Designs.Nodes[alias], vars, extra)
		if err != nil {
			return e.WithMessage(err, "compile node design for %s", alias)
		}

		designs[alias] += strings.TrimSpace(bn) + "\n"

		log.Debug().Str("node", alias).Interface("design", designs[alias]).Msg("node design generated")
	}

	genesis, err := contest.CompileTemplate(cmd.design.Designs.Genesis, vars, nil)
	if err != nil {
		return e.WithMessage(err, "compile genesis design")
	}

	genesisfile := filepath.Join(cmd.basedir, "genesis.yml")

	f, err := os.OpenFile(genesisfile, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return errors.Wrapf(err, "create genesis file for %q", genesisfile)
	}

	if _, err := f.WriteString(genesis); err != nil {
		return errors.Wrapf(err, "create genesis file for %q", genesisfile)
	}

	if err := cmd.hosts.TraverseByHost(func(h contest.Host, _ []string) (bool, error) {
		if err := h.Upload(strings.NewReader(genesis), "genesis.yml", "genesis.yml", 0o600); err != nil {
			return false, e.Wrap(err)
		}

		return true, nil
	}); err != nil {
		return e.WithMessage(err, "upload genesis.yml")
	}

	for alias := range designs {
		host := cmd.hosts.HostByContainer(containerName(alias))
		if host == nil {
			return e.Errorf("not found in host")
		}

		configfile := filepath.Join(cmd.basedir, alias+".yml")

		if err := func() error {
			f, err := os.OpenFile(configfile, os.O_WRONLY|os.O_CREATE, 0o600)
			if err != nil {
				return errors.Wrapf(err, "create node design file for %q", alias)
			}

			defer func() {
				_ = f.Close()
			}()

			if _, err := f.WriteString(designs[alias]); err != nil {
				return errors.Wrapf(err, "write node design file for %q", alias)
			}

			return nil
		}(); err != nil {
			return e.Wrap(err)
		}

		if err := host.Mkdir(
			alias,
			0o700,
		); err != nil {
			return e.Wrap(err)
		}

		if err := host.Upload(
			strings.NewReader(designs[alias]),
			"config.yml",
			filepath.Join(alias, "config.yml"),
			0o600,
		); err != nil {
			return e.Wrap(err)
		}
	}

	cmd.vars = vars

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
			return false, errors.WithStack(err)
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
			locals[i].(*contest.LocalHost).SetPublishHost(remoteside.String()) //nolint:forcetypeassert //...
		}
	}

	return nil
}

func (*runCommand) checkImages(client *dockerClient.Client, images ...string) error {
	for i := range images {
		image := images[i]

		switch found, err := contest.ExistsImage(client, image); {
		case err != nil:
			return errors.WithMessagef(err, "check image, %q", image)
		case !found:
			if err := contest.PullImage(client, image); err != nil {
				return errors.WithMessagef(err, "pull image, %q", image)
			}
		}
	}

	return nil
}
