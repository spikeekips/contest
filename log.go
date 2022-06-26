package contest

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spikeekips/mitum/util"
	"github.com/spikeekips/mitum/util/logging"
	"go.mongodb.org/mongo-driver/bson"
)

type WatchLogs struct {
	*logging.Logging
	*util.ContextDaemon
	actionFunc           func(context.Context, ScenarioAction) error
	insertLogEntriesFunc func(context.Context, []LogEntry) error
	vars                 *Vars
	getHostFunc          func(string) Host
	findDBFunc           func(context.Context, bson.M) (interface{}, bool, error)
	expects              []ExpectScenario
	actives              []ExpectScenario
	checkInterval        time.Duration
}

func NewWatchLogs(
	expects []ExpectScenario,
	savelogch chan LogEntry,
	checkInterval *time.Duration,
	vars *Vars,
	getHostFunc func(string) Host,
	findDBFunc func(context.Context, bson.M) (interface{}, bool, error),
	actionFunc func(context.Context, ScenarioAction) error,
	insertLogEntriesFunc func(context.Context, []LogEntry) error,
) *WatchLogs {
	ucheckInterval := time.Millisecond * 300 //nolint:gomnd //...
	if checkInterval != nil {
		ucheckInterval = *checkInterval
	}

	actives := make([]ExpectScenario, len(expects))
	copy(actives, expects)

	w := &WatchLogs{
		Logging: logging.NewLogging(func(zctx zerolog.Context) zerolog.Context {
			return zctx.Str("module", "watch-logs")
		}),
		expects:              expects,
		actives:              actives,
		checkInterval:        ucheckInterval,
		vars:                 vars,
		getHostFunc:          getHostFunc,
		findDBFunc:           findDBFunc,
		actionFunc:           actionFunc,
		insertLogEntriesFunc: insertLogEntriesFunc,
	}

	w.ContextDaemon = util.NewContextDaemon(func(ctx context.Context) error {
		return w.start(ctx, savelogch)
	})

	return w
}

func (w *WatchLogs) start(ctx context.Context, savelogch chan LogEntry) error {
	go w.saveLogs(ctx, savelogch)

	ticker := time.NewTicker(w.checkInterval)
	defer ticker.Stop()

	active, queries, err := w.newactive()
	if err != nil {
		return err
	}

	w.Log().Debug().Stringer("query", queries[0]).Msg("querying")

end:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			switch left, ok, err := w.evaluate(ctx, active, queries); {
			case err != nil:
				return err
			case !ok:
				continue end
			case len(w.actives) < 1: // NOTE finished
				break end
			case len(left) < 1:
				active, queries, err = w.newactive()
				if err != nil {
					return err
				}

				w.Log().Debug().Stringer("query", queries[0]).Msg("querying")
			default:
				queries = left

				w.Log().Debug().Stringer("query", queries[0]).Msg("querying")
			}
		}
	}

	w.Log().Info().Msg("finished")

	return nil
}

func (w *WatchLogs) newactive() (newactive ExpectScenario, queires []ConditionQuery, _ error) {
	selected, left := w.actives[0], w.actives[1:]

	active, err := selected.Compile(w.vars)
	if err != nil {
		return active, nil, err
	}

	queries, err := w.compileConditionQueries(active)
	if err != nil {
		return active, nil, err
	}

	w.Log().Debug().
		Interface("expect", active).
		Func(func(e *zerolog.Event) {
			s := make([]fmt.Stringer, len(queries))

			for i := range s {
				s[i] = queries[i]
			}

			e.Stringers("queries", s)
		}).
		Msg("new expect")

	w.actives = left

	return active, queries, nil
}

func (w *WatchLogs) compileConditionQueries(expect ExpectScenario) (queries []ConditionQuery, _ error) {
	if len(expect.Range) < 1 {
		query, err := w.compileConditionQuery(expect.Condition, w.vars)
		if err != nil {
			return nil, err
		}

		return []ConditionQuery{query}, nil
	}

	rv := expect.RangeValues()

	queries = make([]ConditionQuery, len(rv))
	for i := range rv {
		vars := w.vars.Clone(nil)
		vars.Set(".self.range", rv[i])

		query, err := w.compileConditionQuery(expect.Condition, vars)
		if err != nil {
			return nil, err
		}

		queries[i] = query
	}

	return queries, nil
}

func (w *WatchLogs) compileConditionQuery(s string, vars *Vars) (ConditionQuery, error) {
	e := util.StringErrorFunc("failed to compile condition query")

	var alias string
	var rangevalue map[string]interface{}

	switch i, found := vars.Value(".self.range"); {
	case !found:
		rangevalue = map[string]interface{}{}
	default:
		rangevalue = i.(map[string]interface{}) //nolint:forcetypeassert //...

		if j, found := rangevalue["node"]; found {
			alias = j.(string) //nolint:forcetypeassert //...
		}
	}

	switch n := strings.TrimLeft(s, " "); {
	case strings.HasPrefix(n, "{"):
		c, err := CompileTemplate(n, vars, nil)
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

		return MongodbConditionQuery{findDBFunc: w.findDBFunc, m: m}, nil
	case strings.HasPrefix(n, "$"):
		if len(alias) < 1 {
			return nil, e(nil, "empty alias for hostCommandConditionQuery, %q", s)
		}

		host := w.getHostFunc(alias)
		if host == nil {
			return nil, e(nil, "host not found")
		}

		vars.Set(".self.host", host)

		c, err := CompileTemplate(n[1:], vars, nil)
		if err != nil {
			return nil, e(err, "")
		}

		return HostCommandConditionQuery{host: host, cmd: c}, nil
	default:
		return nil, e(nil, "unknown condition, %q", s)
	}
}

func (w *WatchLogs) evaluate(
	ctx context.Context, expect ExpectScenario, queries []ConditionQuery,
) (left []ConditionQuery, ok bool, _ error) {
	current, err := expect.Compile(w.vars)
	if err != nil {
		return left, false, err
	}

	query := queries[0]

	l := w.Log().With().Stringer("query", query).Logger()

	if len(queries) > 0 {
		left = queries[1:]
	}

	r, found, err := query.Find(ctx)
	switch {
	case err != nil:
		return left, false, err
	case !found:
		return left, false, nil
	default:
		ok = found

		l.Trace().Interface("out", r).Msg("matched")
	}

	for i := range current.Registers {
		register := current.Registers[i]

		l := w.Log().With().Interface("result", r).Interface("register", register).Logger()

		if err := w.register(r, register); err != nil {
			l.Error().Err(err).Msg("failed to register")

			return left, ok, err
		}

		l.Debug().Msg("registered")
	}

	for i := range current.Actions {
		action := current.Actions[i]

		l := w.Log().With().Interface("action", action).Logger()

		if err := w.actionFunc(ctx, action); err != nil {
			l.Error().Err(err).Msg("failed to run action")

			return left, ok, err
		}

		l.Debug().Msg("action done")
	}

	return left, true, nil
}

func (w *WatchLogs) register(record interface{}, register ScenarioRegister) error {
	w.vars.Set(register.Assign, record)

	return nil
}

func (w *WatchLogs) saveLogs(ctx context.Context, ch chan LogEntry) {
	var entries []LogEntry

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

end:
	for {
		select {
		case <-ctx.Done():
			return
		case e, notclosed := <-ch:
			if !notclosed {
				break end
			}

			entries = append(entries, e)
		case <-ticker.C:
			if len(entries) > 0 {
				if err := w.insertLogEntriesFunc(ctx, entries); err != nil {
					log.Error().Err(err).Msg("failed to save logs")
				}

				entries = nil
			}
		}
	}

	if len(entries) > 0 {
		if err := w.insertLogEntriesFunc(ctx, entries); err != nil {
			log.Error().Err(err).Msg("failed to save logs")
		}
	}
}

type ConditionQuery interface {
	fmt.Stringer
	Find(context.Context) (interface{}, bool, error)
}

type MongodbConditionQuery struct {
	findDBFunc func(context.Context, bson.M) (interface{}, bool, error)
	m          bson.M
}

func (c MongodbConditionQuery) String() string {
	b, _ := util.MarshalJSON(c.m)

	return string(b)
}

func (c MongodbConditionQuery) Find(ctx context.Context) (interface{}, bool, error) {
	return c.findDBFunc(ctx, c.m)
}

type HostCommandConditionQuery struct {
	host Host
	cmd  string
}

func (c HostCommandConditionQuery) String() string {
	return c.cmd
}

func (c HostCommandConditionQuery) Find(ctx context.Context) (interface{}, bool, error) {
	out, ok, err := c.host.RunCommand(c.cmd)
	if err != nil {
		return nil, false, nil
	}

	return strings.TrimRight(out, "\n"), ok, nil
}
