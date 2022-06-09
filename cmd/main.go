package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	contest "github.com/spikeekips/contest2"
	_ "github.com/spikeekips/mitum/launch"
	"github.com/spikeekips/mitum/util"
	"github.com/spikeekips/mitum/util/localtime"
	mitumlogging "github.com/spikeekips/mitum/util/logging"
	"gopkg.in/yaml.v3"
)

var (
	logging *mitumlogging.Logging
	log     *zerolog.Logger
)

var contestID = util.ULID().String()

var (
	defaultMongodbURI = "mongodb://localhost:27017/contest_" + contestID
	defaultDockerHost = &url.URL{Host: "local"}
)

var kongOptions = []kong.Option{
	kong.Name("contest"),
	kong.Vars{
		"mongodb_uri": defaultMongodbURI,
	},
}

func main() {
	logging = mitumlogging.Setup(os.Stderr, zerolog.DebugLevel, "json", false)
	log = mitumlogging.NewLogging(func(lctx zerolog.Context) zerolog.Context {
		return lctx.Str("module", "main")
	}).SetLogging(logging).Log()

	var cli struct {
		Run runCommand `cmd:"" help:"run contest"`
	}

	kctx := kong.Parse(&cli, kongOptions...)

	log.Info().Str("command", kctx.Command()).Msg("start command")

	if err := func() error {
		defer log.Info().Msg("stopped")

		return errors.Wrap(kctx.Run(), "")
	}(); err != nil {
		log.Error().Err(err).Msg("stopped by error")

		kctx.FatalIfErrorf(err)
	}
}

type runCommand struct {
	BaseDir  string     `arg:"" name:"base_directory" help:"base directory"`
	Scenario string     `arg:"" name:"scenario" help:"scenario file" type:"existingfile"`
	Hosts    []*url.URL `arg:"" name:"host" help:"docker host"`
	Mongodb  string     `name:"mongodb" help:"mongodb uri" default:"${mongodb_uri}"`
	db       *contest.Mongodb
	id       string
	basedir  string
	scenario contest.Scenario
	vars     *contest.Vars
}

func (cmd *runCommand) Run() error {
	if err := cmd.prepare(); err != nil {
		return errors.Wrap(err, "")
	}

	logch := make(chan contest.LogEntry)
	defer close(logch)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go cmd.saveLogs(ctx, logch)

	logch <- contest.NewInternalLogEntry("contest ready", nil)

	if err := cmd.watchLogs(ctx); err != nil {
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

	if err := cmd.prepareScenario(); err != nil {
		return errors.Wrap(err, "")
	}

	log.Debug().Interface("vars", cmd.vars.Map()).Msg("vars")

	return nil
}

func (cmd *runCommand) prepareFlags() error {
	switch {
	case len(cmd.Hosts) < 1:
		cmd.Hosts = []*url.URL{defaultDockerHost}
	default:
		e := util.StringErrorFunc("invalid docker host")
		for i := range cmd.Hosts {
			switch h := cmd.Hosts[i]; {
			case h.Scheme != "tcp":
				return e(nil, "scheme is not tcp, %q", h)
			case len(h.Host) < 1:
				return e(nil, "empty host")
			case len(h.Port()) < 1:
				h.Host = fmt.Sprintf("%s:2376", h.Host)
			}
		}
	}

	log.Debug().
		Str("id", contestID).
		Str("basedir", cmd.BaseDir).
		Func(func(e *zerolog.Event) {
			hosts := make([]fmt.Stringer, len(cmd.Hosts))
			for i := range cmd.Hosts {
				hosts[i] = cmd.Hosts[i]
			}

			e.Stringers("hosts", hosts)
		}).
		Msg("flags")

	return nil
}

func (cmd *runCommand) prepareBase() error {
	e := util.StringErrorFunc("failed to prepare base directory")

	scenario := filepath.Base(cmd.Scenario)
	scenario = scenario[:len(scenario)-len(filepath.Ext(scenario))]

	base := filepath.Join(cmd.BaseDir, fmt.Sprintf("%s-%s-%s", contestID, localtime.RFC3339(localtime.Now()), scenario))

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

	if err := yaml.Unmarshal([]byte(i), &cmd.scenario); err != nil {
		return e(err, "")
	}

	log.Debug().Interface("scenario", cmd.scenario).Msg("scenario loaded")

	vars := contest.NewVars(nil)

	// NOTE global vars
	for k := range cmd.scenario.Vars {
		vars.Set(k, cmd.scenario.Vars[k])
	}

	vars = vars.AddFunc("nodePublishAddr", contest.NodePublishAddr)

	// NOTE nodes design
	designs := map[string]string{}

	for alias := range cmd.scenario.Designs.Nodes {
		extra := map[string]interface{}{
			"self": map[string]interface{}{
				"alias": alias,
			},
		}

		bc, err := contest.CompileTemplate(cmd.scenario.Designs.Common, vars, extra)
		if err != nil {
			return e(err, "failed to compile common design for %s", alias)
		}

		bn, err := contest.CompileTemplate(cmd.scenario.Designs.Nodes[alias], vars, extra)
		if err != nil {
			return e(err, "failed to compile node design for %s", alias)
		}

		designs[alias] = strings.TrimSpace(bc+"\n"+bn) + "\n"

		vars.Rename(".self", ".nodes."+alias)

		log.Debug().Str("node", alias).Interface("design", designs[alias]).Msg("node design generated")
	}

	for alias := range designs {
		f, err := os.OpenFile(filepath.Join(cmd.basedir, alias+".yml"), os.O_WRONLY|os.O_CREATE, 0o600)
		if err != nil {
			return e(err, "failed to create node design file for %q", alias)
		}

		if _, err := f.WriteString(designs[alias]); err != nil {
			return e(err, "failed to write node design file for %q", alias)
		}
	}

	cmd.vars = vars

	return nil
}

func (cmd *runCommand) watchLogs(ctx context.Context) error {
	expects := make([]contest.ExpectScenario, len(cmd.scenario.Expects))
	copy(expects, cmd.scenario.Expects)

	var active contest.ExpectScenario

	newactive := func() error {
		var nactive contest.ExpectScenario

		nactive, expects = expects[0], expects[1:]
		nactive, err := nactive.Compile(cmd.vars)
		if err != nil {
			return err
		}

		active = nactive

		log.Debug().Interface("expect", active).Msg("new expect")

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
			switch ok, err := cmd.evaluateLog(active); {
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

func (cmd *runCommand) evaluateLog(expect contest.ExpectScenario) (bool, error) {
	var ok bool

	for i := range expect.Registers {
		if err := cmd.register(expect.Registers[i]); err != nil {
			return ok, errors.Wrap(err, "")
		}
	}

	return true, nil
}

func (cmd *runCommand) register(register contest.RegisterScenario) error {
	cmd.vars.Set(register.Assign, "showme") // FIXME hehehe

	return nil
}

func (cmd *runCommand) saveLogs(ctx context.Context, ch chan contest.LogEntry) {
	var entries []contest.LogEntry

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ch:
			entries = append(entries, e)
		case <-ticker.C:
			if len(entries) < 1 {
				continue
			}

			if err := cmd.db.InsertLogEntries(ctx, entries); err != nil {
				log.Error().Err(err).Msg("failed to save logs")

				continue
			}

			entries = nil
		}
	}
}
