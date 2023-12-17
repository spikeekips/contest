package contest

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/oleksandr/conditions"
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
	countDBFunc          func(context.Context, bson.M) (int64, error)
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
	countDBFunc func(context.Context, bson.M) (int64, error),
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
		countDBFunc:          countDBFunc,
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

	active, queries, err := w.newactive()
	if err != nil {
		return err
	}

	if len(queries) > 0 {
		w.Log().Debug().Stringer("query", queries[0]).Msg("querying")
	}

	if len(active.Log) > 0 {
		w.expectLog(active.Log)
	}

	interval := w.checkInterval
	initialWait := active.InitialWait

end:
	for {
		var updated bool

		if initialWait > 0 {
			w.Log().Debug().Dur("initial_wait", initialWait).Msg("initial wait")

			<-time.After(initialWait)
		}

		switch left, ok, err := w.evaluate(ctx, active, queries); {
		case err != nil:
			return err
		case !ok:
		case len(w.actives) < 1: // NOTE finished
			break end
		case len(left) < 1:
			active, queries, err = w.newactive()
			if err != nil {
				return err
			}

			if len(active.Log) > 0 {
				w.expectLog(active.Log)
			}

			if active.Interval > 1 {
				interval = active.Interval
			}

			initialWait = active.InitialWait
			updated = true
		default:
			queries = left
			updated = true
		}

		if updated && len(queries) > 0 {
			w.Log().Debug().Dur("interval", interval).Stringer("query", queries[0]).Msg("querying")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}

	w.Log().Info().Msg("finished")

	return nil
}

func (w *WatchLogs) newactive() (newactive ExpectScenario, queries []ConditionQuery, _ error) {
	selected, left := w.actives[0], w.actives[1:]

	active, err := selected.Compile(w.vars)
	if err != nil {
		w.Log().Error().
			Err(err).
			Interface("selected", selected).
			Msg("failed to compile expect")

		return active, nil, err
	}

	if len(active.Log) > 0 {
		w.actives = left

		return active, nil, nil
	}

	qs, err := w.compileConditionQueries(active)
	if err != nil {
		w.Log().Error().
			Err(err).
			Interface("selected", selected).
			Msg("failed to compile query")

		return active, nil, err
	}

	w.Log().Debug().
		Interface("expect", active).
		Func(func(e *zerolog.Event) {
			s := make([]fmt.Stringer, len(qs))

			for i := range s {
				s[i] = qs[i]
			}

			e.Stringers("queries", s)
		}).
		Msg("new expect")

	w.actives = left

	return active, qs, nil
}

func (w *WatchLogs) compileConditionQueries(expect ExpectScenario) (queries []ConditionQuery, _ error) {
	if len(expect.Range) < 1 {
		query, err := w.compileConditionQuery(expect.Condition, w.vars, nil)
		if err != nil {
			return nil, err
		}

		return []ConditionQuery{query}, nil
	}

	rv := expect.RangeValues()

	queries = make([]ConditionQuery, len(rv))

	for i := range rv {
		query, err := w.compileConditionQuery(expect.Condition, w.vars.Clone(nil), rv[i])
		if err != nil {
			return nil, err
		}

		queries[i] = query
	}

	return queries, nil
}

func (w *WatchLogs) compileConditionQuery(
	s interface{}, vars *Vars, rangeValue map[string]interface{},
) (ConditionQuery, error) {
	switch t := s.(type) {
	case string:
		return w.compileStringConditionQuery(t, vars, rangeValue)
	case map[string]interface{}:
		return w.compileMapConditionQuery(t, vars, rangeValue)
	default:
		return nil, errors.Errorf("unknown condition type, %T", t)
	}
}

func (w *WatchLogs) compileStringConditionQuery(
	s string, vars *Vars, rangeValue map[string]interface{},
) (ConditionQuery, error) {
	e := util.StringError("compile condition string query")

	var alias string

	if rangeValue != nil {
		if j, found := rangeValue["node"]; found {
			alias = j.(string) //nolint:forcetypeassert //...
		}

		switch i, found := vars.Value(".nodes." + alias); {
		case !found:
			return nil, e.Errorf("node vars not found")
		default:
			vars.Set(".self", i)
			vars.Set(".self.range", rangeValue)
		}
	}

	switch n := strings.TrimLeft(s, " "); {
	case strings.HasPrefix(n, "{"):
		c, err := CompileTemplate(n, vars, nil)
		if err != nil {
			return nil, e.Wrap(err)
		}

		var m bson.M
		if err := bson.UnmarshalExtJSON([]byte(c), false, &m); err != nil {
			return nil, errors.WithMessagef(err, "unmarshal query, %q", c)
		}

		for k := range rangeValue {
			m[k] = rangeValue[k]
		}

		return MongodbFindConditionQuery{findDBFunc: w.findDBFunc, m: m}, nil
	case strings.HasPrefix(n, "$ "):
		if len(alias) < 1 {
			return nil, e.Errorf("empty alias for hostCommandConditionQuery, %q", s)
		}

		host := w.getHostFunc(alias)
		if host == nil {
			return nil, e.Errorf("host not found")
		}

		vars.Set(".self.host", host)

		c, err := CompileTemplate(n[1:], vars, nil)
		if err != nil {
			return nil, e.Wrap(err)
		}

		return HostCommandConditionQuery{host: host, cmd: c}, nil
	default:
		return nil, e.Errorf("unknown condition, %q", s)
	}
}

func (w *WatchLogs) compileMapConditionQuery(
	s map[string]interface{}, vars *Vars, rangeValue map[string]interface{},
) (ConditionQuery, error) {
	e := util.StringError("compile condition map query")

	var query, countString string
	var count conditions.Expr

	for key := range s {
		var value string

		switch t := s[key].(type) {
		case string:
			value = t
		default:
			return nil, e.Errorf("unknown map condition value type, %T", t)
		}

		switch key {
		case "query":
			query = value
		case "count":
			p := conditions.NewParser(strings.NewReader(fmt.Sprintf("$0 %s", value)))

			expr, err := p.Parse()
			if err != nil {
				return nil, e.Wrap(err)
			}

			count = expr
			countString = value
		default:
			return nil, e.Errorf("unknown map condition key, %q", key)
		}
	}

	vars.Set(".self.range", rangeValue)

	if count == nil {
		return w.compileStringConditionQuery(query, vars, rangeValue)
	}

	c, err := CompileTemplate(query, vars, nil)
	if err != nil {
		return nil, e.Wrap(err)
	}

	var m bson.M
	if err := bson.UnmarshalExtJSON([]byte(c), false, &m); err != nil {
		return nil, errors.WithMessagef(err, "unmarshal query, %q", c)
	}

	for k := range rangeValue {
		m[k] = rangeValue[k]
	}

	return MongodbCountConditionQuery{
		countDBFunc: w.countDBFunc,
		m:           m,
		count:       count,
		countString: countString,
	}, nil
}

func (w *WatchLogs) evaluate(
	ctx context.Context, expect ExpectScenario, queries []ConditionQuery,
) (left []ConditionQuery, ok bool, _ error) {
	if len(expect.Log) > 0 {
		return nil, true, nil
	}

	current, err := expect.Compile(w.vars)
	if err != nil {
		return left, false, err
	}

	query := queries[0]

	l := w.Log().With().Stringer("query", query).Logger()

	if len(queries) > 0 {
		left = queries[1:]
	}

	var r interface{}

	switch i, found, err := query.Find(ctx); {
	case err != nil:
		return left, false, errors.WithStack(err)
	case !found:
		var errstring string
		if j, isstring := i.(string); isstring {
			errstring = j
		}

		return left, false, w.ifConditionFailed(ctx, current, errstring)
	default:
		ok = found

		l.Debug().Interface("out", i).Msg("matched")

		r = i
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
	var v interface{}

	switch {
	case register.Format == "json":
		s, ok := record.(string)
		if !ok {
			return errors.Errorf("format json; expected string, but %T", record)
		}

		if err := util.UnmarshalJSON([]byte(s), &v); err != nil {
			return err
		}
	default:
		v = record
	}

	w.vars.Set(register.Assign, v)

	return nil
}

func (w *WatchLogs) saveLogs(ctx context.Context, ch chan LogEntry) {
	var entries []LogEntry

	save := func() {
		if len(entries) < 1 {
			return
		}

		if err := w.insertLogEntriesFunc(ctx, entries); err != nil {
			log.Error().Err(err).Msg("failed to save logs")
		}

		entries = nil
	}

	ticker := time.NewTicker(time.Millisecond * 33)
	defer ticker.Stop()

end:
	for {
		select {
		case <-ctx.Done():
			break end
		case e, notclosed := <-ch:
			if !notclosed {
				break end
			}

			entries = append(entries, e)

			if len(entries) > 33 { //nolint:gomnd //...
				save()
			}
		case <-ticker.C:
			save()
		}
	}

	save()
}

func (w *WatchLogs) ifConditionFailed(ctx context.Context, scenario ExpectScenario, err string) error {
	switch scenario.IfConditionFailed {
	case IfConditionFailedNothing:
	case IfConditionFailedStopContest:
		return w.actionFunc(ctx, ScenarioAction{Type: "stop-contest", Args: []string{err}})
	}

	return nil
}

func (w *WatchLogs) expectLog(s string) {
	w.Log().Info().Msgf("expect log: %s", s)
}

type ConditionQuery interface {
	fmt.Stringer
	Find(context.Context) (interface{}, bool, error)
}

type MongodbFindConditionQuery struct {
	findDBFunc func(context.Context, bson.M) (interface{}, bool, error)
	m          bson.M
}

func (c MongodbFindConditionQuery) String() string {
	b, _ := util.MarshalJSON(c.m)

	return string(b)
}

func (c MongodbFindConditionQuery) Find(ctx context.Context) (out interface{}, ok bool, _ error) {
	return c.findDBFunc(ctx, c.m)
}

type MongodbCountConditionQuery struct {
	count       conditions.Expr
	countDBFunc func(context.Context, bson.M) (int64, error)
	m           bson.M
	countString string
}

func (c MongodbCountConditionQuery) String() string {
	b, _ := util.MarshalJSON(map[string]interface{}{
		"query": c.m,
		"count": c.countString,
	})

	return string(b)
}

func (c MongodbCountConditionQuery) Find(ctx context.Context) (out interface{}, ok bool, _ error) {
	i, err := c.countDBFunc(ctx, c.m)
	if err != nil {
		return nil, false, err
	}

	r, err := conditions.Evaluate(c.count, map[string]interface{}{"$0": i})

	return nil, r, errors.WithStack(err)
}

type HostCommandConditionQuery struct {
	host Host
	cmd  string
}

func (c HostCommandConditionQuery) String() string {
	return c.cmd
}

func (c HostCommandConditionQuery) Find(context.Context) (out interface{}, ok bool, _ error) {
	stdout, stderr, ok, err := c.host.RunCommand(c.cmd)
	if err != nil {
		return nil, false, err
	}

	outstring := strings.TrimRight(stdout, "\n")
	if !ok {
		outstring += "\n\n" + strings.TrimRight(stderr, "\n")
	}

	return outstring, ok, nil
}
