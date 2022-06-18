package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	dockerTypes "github.com/docker/docker/api/types"
	dockerstdcopy "github.com/docker/docker/pkg/stdcopy"
	"github.com/nxadm/tail"
	"github.com/pkg/errors"
	contest "github.com/spikeekips/contest2"
	"github.com/spikeekips/mitum/util"
	"go.mongodb.org/mongo-driver/bson"
)

func (cmd *runCommand) watchLogs(ctx context.Context) error {
	expects := make([]contest.ExpectScenario, len(cmd.design.Expects))
	copy(expects, cmd.design.Expects)

	var active contest.ExpectScenario
	var queries []conditionQuery

	newactive := func() error {
		active, expects = expects[0], expects[1:]

		i, err := active.Compile(cmd.vars)
		if err != nil {
			return errors.Wrap(err, "")
		}

		active = i

		q, err := cmd.compileConditionQueries(active)
		if err != nil {
			return errors.Wrap(err, "")
		}

		queries = q

		log.Debug().Interface("expect", active).Msg("new expect")

		return nil
	}

	if err := newactive(); err != nil {
		return errors.Wrap(err, "")
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

end:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			switch left, ok, err := cmd.evaluateLog(ctx, active, queries); {
			case err != nil:
				return errors.Wrap(err, "")
			case ok:
				if len(expects) < 1 { // NOTE finished
					break end
				}

				queries = left

				if len(queries) < 1 {
					if err := newactive(); err != nil {
						return errors.Wrap(err, "")
					}
				}
			}
		}
	}

	log.Info().Msg("finished")

	return nil
}

func (cmd *runCommand) compileConditionQueries(expect contest.ExpectScenario) (queries []conditionQuery, _ error) {
	if len(expect.Range) < 1 {
		query, err := cmd.compileConditionQuery(expect.Condition, cmd.vars)
		if err != nil {
			return nil, errors.Wrap(err, "")
		}

		return []conditionQuery{query}, nil
	}

	for k := range expect.Range {
		queries = make([]conditionQuery, len(expect.Range[k]))

		for i := range expect.Range[k] {
			vars := cmd.vars.Clone(nil)
			vars.Set(".self.range."+k, expect.Range[k][i])

			query, err := cmd.compileConditionQuery(expect.Condition, vars)
			if err != nil {
				return nil, errors.Wrap(err, "")
			}

			queries[i] = query
		}

		break
	}

	return queries, nil
}

func (cmd *runCommand) compileConditionQuery(s string, vars *contest.Vars) (conditionQuery, error) {
	e := util.StringErrorFunc("failed to compile condition query")

	var alias string
	var rangevalue map[string]interface{}

	switch i, found := vars.Value(".self.range"); {
	case !found:
		rangevalue = map[string]interface{}{}
	default:
		rangevalue = i.(map[string]interface{})

		if j, found := rangevalue["node"]; found {
			alias = j.(string)
		}
	}

	switch n := strings.TrimLeft(s, " "); {
	case strings.HasPrefix(n, "{"):
		c, err := contest.CompileTemplate(n, vars, nil)
		if err != nil {
			return nil, e(err, "")
		}

		var m bson.M
		if err := bson.UnmarshalExtJSON([]byte(c), false, &m); err != nil {
			return nil, errors.Wrap(err, "")
		}

		for k := range rangevalue {
			m[k] = rangevalue[k]
		}

		return dbConditionQuery{db: cmd.db, m: m}, nil
	case strings.HasPrefix(n, "$"):
		if len(alias) < 1 {
			return nil, e(nil, "empty alias for hostCommandConditionQuery, %q", s)
		}

		host := cmd.hosts.HostByContainer(containerName(alias))
		if host == nil {
			return nil, e(nil, "host not found")
		}

		vars.Set(".self.host", host)

		c, err := contest.CompileTemplate(n[1:], vars, nil)
		if err != nil {
			return nil, e(err, "")
		}

		return hostCommandConditionQuery{host: host, cmd: c}, nil
	default:
		return nil, e(nil, "unknown condition, %q", s)
	}
}

func (cmd *runCommand) evaluateLog(ctx context.Context, expect contest.ExpectScenario, queries []conditionQuery) (left []conditionQuery, ok bool, _ error) {
	current, err := expect.Compile(cmd.vars)
	if err != nil {
		return left, false, errors.Wrap(err, "")
	}

	query := queries[0]

	l := log.With().Str("query", query.query()).Logger()

	l.Debug().Msg("querying")

	if len(queries) > 0 {
		left = queries[1:]
	}

	r, found, err := query.find(ctx)
	switch {
	case err != nil:
		return left, false, errors.Wrap(err, "")
	case !found:
		return left, false, nil
	default:
		ok = found

		l.Debug().Msg("matched")
	}

	for i := range current.Actions {
		action := current.Actions[i]

		l := log.With().Stringer("logid", util.UUID()).Logger()
		l.Debug().Interface("action", action).Msg("trying to run action")

		if err := cmd.action(ctx, action); err != nil {
			l.Error().Err(err).Msg("failed to run action")

			return left, ok, errors.Wrap(err, "")
		}

		l.Debug().Msg("action done")
	}

	for i := range current.Registers {
		register := current.Registers[i]

		l := log.With().Stringer("registerid", util.UUID()).Logger()
		l.Debug().Interface("register", register).Msg("trying to register")

		if err := cmd.register(r, register); err != nil {
			l.Error().Err(err).Msg("failed to register")

			return left, ok, errors.Wrap(err, "")
		}

		l.Debug().Msg("register done")
	}

	return left, true, nil
}

func (cmd *runCommand) register(
	record interface{}, register contest.ScenarioRegister,
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

type conditionQuery interface {
	query() string
	find(context.Context) (interface{}, bool, error)
}

type dbConditionQuery struct {
	db *contest.Mongodb
	m  bson.M
}

func (c dbConditionQuery) query() string {
	b, _ := util.MarshalJSON(c.m)

	return string(b)
}

func (c dbConditionQuery) find(ctx context.Context) (interface{}, bool, error) {
	return c.db.Find(ctx, c.m)
}

type hostCommandConditionQuery struct {
	cmd  string
	host contest.Host
}

func (c hostCommandConditionQuery) query() string {
	return c.cmd
}

func (c hostCommandConditionQuery) find(ctx context.Context) (interface{}, bool, error) {
	out, ok, err := c.host.RunCommand(c.cmd)
	if err != nil {
		return nil, false, nil
	}

	return out, ok, nil
}
