package contest

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/spikeekips/mitum/util"
)

type Design struct {
	Vars                        map[string]interface{} `yaml:"vars"`
	Designs                     NodeDesigns            `yaml:"designs"`
	Expects                     []ExpectScenario       `yaml:"expects"`
	Nodes                       NodesDesign            `yaml:"nodes"`
	IgnoreAbnormalContainerExit bool                   `yaml:"ignore_abnormal_container_exit"`
}

func (s Design) IsValid(b []byte) error {
	e := util.StringError("invalid Design")

	if err := s.Designs.IsValid(b); err != nil {
		return e.Wrap(err)
	}

	if len(s.Expects) < 1 {
		return e.Errorf("empty expects")
	}

	for i := range s.Expects {
		if err := s.Expects[i].IsValid(b); err != nil {
			return e.Wrap(err)
		}
	}

	if util.IsDuplicatedSlice(s.Nodes.SameHost, func(i string) (bool, string) {
		return true, i //nolint:forcetypeassert //...
	}) {
		return e.Errorf("duplicated nodes found")
	}

	return nil
}

type NodeDesigns struct {
	Common      string            `yaml:"common"`
	NumberNodes *int              `yaml:"number_nodes"`
	Nodes       map[string]string `yaml:"nodes"`
	Genesis     string            `yaml:"genesis"`
}

func (s NodeDesigns) IsValid([]byte) error {
	e := util.StringError("invalid NodeDesigns")

	if (s.NumberNodes == nil || *s.NumberNodes < 1) && len(s.Nodes) < 1 {
		return e.Errorf("empty nodes")
	}

	if len(s.Nodes) > 0 {
		// NOTE check node alias format
		for i := range s.Nodes {
			if err := isValidNodeAliasFormat(i); err != nil {
				return e.Wrap(err)
			}
		}
	}

	return nil
}

func (s NodeDesigns) AllNodes() []string {
	var num int

	if s.NumberNodes != nil {
		num = *s.NumberNodes
	}

	if num < 1 { // NOTE find number of nodes from s.Nodes
		aliases := make([]string, len(s.Nodes))

		var i int

		for alias := range s.Nodes {
			aliases[i] = alias
			i++
		}

		sort.Slice(aliases, func(i, j int) bool {
			var ni, nj int

			if _, err := fmt.Sscanf(aliases[i], "no%d", &ni); err != nil {
				return false
			}

			if _, err := fmt.Sscanf(aliases[j], "no%d", &nj); err != nil {
				return false
			}

			return ni > nj
		})

		var n int
		if _, err := fmt.Sscanf(aliases[0], "no%d", &n); err != nil {
			panic(err)
		}

		num = n + 1
	}

	nodes := make([]string, num)

	for i := range nodes {
		nodes[i] = nodeAlias(i)
	}

	return nodes
}

type IfConditionFailedType string

var (
	IfConditionFailedNothing     IfConditionFailedType
	IfConditionFailedStopContest IfConditionFailedType = "stop-contest"
)

func (i IfConditionFailedType) IsValid([]byte) error {
	switch i {
	case IfConditionFailedNothing,
		IfConditionFailedStopContest:
		return nil
	default:
		return errors.Errorf("unknown ifConditionFailedType, %q", i)
	}
}

type ExpectScenario struct {
	Condition         interface{}           `yaml:"condition"`
	Log               string                `yaml:"log"`
	IfConditionFailed IfConditionFailedType `yaml:"if_condition_failed"`
	Range             []map[string][]string `yaml:"range"`
	Actions           []ScenarioAction      `yaml:"actions"`
	Registers         []ScenarioRegister    `yaml:"registers"`
	Interval          time.Duration         `yaml:"interval"`
	InitialWait       time.Duration         `yaml:"initial_wait"`
}

func (s ExpectScenario) IsValid(b []byte) error {
	e := util.StringError("invalid ExpectScenario")

	if s.Log != "" {
		return nil
	}

	if s.Condition == nil {
		return e.Errorf("empty condition")
	}

	if err := s.isValidCondition(); err != nil {
		return e.Wrap(err)
	}

	for i := range s.Actions {
		if err := s.Actions[i].IsValid(b); err != nil {
			return e.Wrap(err)
		}
	}

	for i := range s.Registers {
		if err := s.Registers[i].IsValid(b); err != nil {
			return e.Wrap(err)
		}
	}

	if s.Interval < 0 {
		return e.Errorf("under zero interval")
	}

	if s.InitialWait < 0 {
		return e.Errorf("under zero initial_wait")
	}

	if err := s.IfConditionFailed.IsValid(nil); err != nil {
		return e.Wrap(err)
	}

	return nil
}

func (s ExpectScenario) RangeValues() []map[string]interface{} {
	if len(s.Range) < 1 {
		return nil
	}

	var l int

	if len(s.Range) > 0 {
		for i := range s.Range[0] {
			l = len(s.Range[0][i])

			break
		}
	}

	ms := make([]map[string]interface{}, l)

	for index := range make([]int, l) {
		m := map[string]interface{}{}

		for i := range s.Range {
			for k := range s.Range[i] {
				m[k] = s.Range[i][k][index]
			}
		}

		ms[index] = m
	}

	return ms
}

func (s ExpectScenario) Compile(vars *Vars) (newexpect ExpectScenario, err error) {
	newexpect.Condition = s.Condition
	newexpect.Log = s.Log
	newexpect.Actions = make([]ScenarioAction, len(s.Actions))
	newexpect.Interval = s.Interval
	newexpect.InitialWait = s.InitialWait
	newexpect.IfConditionFailed = s.IfConditionFailed

	copy(newexpect.Actions, s.Actions)

	newexpect.Registers = make([]ScenarioRegister, len(s.Registers))
	for i := range s.Registers {
		newexpect.Registers[i], err = s.Registers[i].Compile(vars)
		if err != nil {
			return newexpect, err
		}
	}

	if len(s.Range) > 0 {
		newexpect.Range = make([]map[string][]string, len(s.Range))

		for i := range s.Range {
			r := s.Range[i]

			m := map[string][]string{}

			for k := range r {
				v := make([]string, len(r[k]))

				for j := range r[k] {
					c, err := CompileTemplate(r[k][j], vars, nil)
					if err != nil {
						return newexpect, err
					}

					v[j] = c
				}

				m[k] = v
			}

			newexpect.Range[i] = m
		}
	}

	return newexpect, nil
}

func (s ExpectScenario) isValidCondition() error {
	switch s.Condition.(type) {
	case string:
	case map[string]interface{}:
	default:
		return errors.Errorf("unknown condition type, %T", s.Condition)
	}

	return nil
}

type ScenarioAction struct {
	Type       string                 `yaml:"type"`
	Properties map[string]interface{} `yaml:"properties"`
	Args       []string               `yaml:"args"`
	Range      []map[string][]string  `yaml:"range"`
}

func (s ScenarioAction) IsValid([]byte) error {
	e := util.StringError("invalid ScenarioAction")

	if len(s.Type) < 1 {
		return e.Errorf("empty type")
	}

	return nil
}

func (s ScenarioAction) RangeValues() []map[string]interface{} {
	if len(s.Range) < 1 {
		return nil
	}

	var l int

	if len(s.Range) > 0 {
		for i := range s.Range[0] {
			l = len(s.Range[0][i])

			break
		}
	}

	ms := make([]map[string]interface{}, l)

	for index := range make([]int, l) {
		m := map[string]interface{}{}

		for i := range s.Range {
			for k := range s.Range[i] {
				m[k] = s.Range[i][k][index]
			}
		}

		ms[index] = m
	}

	return ms
}

func (s ScenarioAction) CompileArgs(vars *Vars) (args []string, err error) {
	args = make([]string, len(s.Args))

	for i := range s.Args {
		args[i], err = CompileTemplate(s.Args[i], vars, nil)
		if err != nil {
			return nil, err
		}
	}

	return args, nil
}

func (s ScenarioAction) CompileProperties(vars *Vars) (_ map[string]interface{}, err error) {
	m := map[string]interface{}{}

	for i := range s.Properties {
		p := s.Properties[i]

		switch t := p.(type) {
		case string:
			m[i], err = CompileTemplate(t, vars, nil)
			if err != nil {
				return nil, err
			}
		default:
			m[i] = p
		}
	}

	return m, nil
}

func ScenarioActionProperty[T any](properties map[string]interface{}, k string, v *T) (bool, error) {
	i, found := properties[k]
	if !found {
		return false, nil
	}

	return true, util.SetInterfaceValue(i, v)
}

type ScenarioRegister struct {
	Type   string `yaml:"type"`
	Assign string `yaml:"assign"`
	Format string `yaml:"format"`
}

func (s ScenarioRegister) IsValid([]byte) error {
	e := util.StringError("invalid ScenarioRegister")

	switch {
	case len(s.Assign) < 1:
		return e.Errorf("empty assign")
	case !strings.HasPrefix(s.Assign, "."):
		return e.Errorf("wrong assign format; must start with `.`")
	case strings.HasSuffix(s.Assign, "."):
		return e.Errorf("wrong assign format; must not end with `.`")
	}

	return nil
}

func (s ScenarioRegister) Compile(vars *Vars) (newregister ScenarioRegister, err error) {
	newregister.Type = s.Type
	newregister.Format = s.Format

	newregister.Assign, err = CompileTemplate(s.Assign, vars, nil)
	if err != nil {
		return newregister, err
	}

	return newregister, nil
}

type NodesDesign struct {
	SameHost []string `yaml:"same_host"`
}

var reNodeAlias = regexp.MustCompile(`^no\d+$`)

func isValidNodeAliasFormat(s string) error {
	e := util.StringError("invalid node alias")

	i := strings.TrimSpace(s)

	switch {
	case len(i) < 1:
		return e.Errorf("empty alias")
	case !reNodeAlias.MatchString(i):
		return e.Errorf("wrong format")
	default:
		return nil
	}
}

func nodeAlias(i int) string {
	return fmt.Sprintf("no%d", i)
}
