package contest

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/spikeekips/mitum/util"
)

type Design struct {
	Vars                            map[string]interface{} `yaml:"vars"`
	NodeDesigns                     NodeDesigns            `yaml:"designs"`
	Expects                         []ExpectScenario       `yaml:"expects"`
	Nodes                           NodesDesign            `yaml:"nodes"`
	IgnoreWhenAbnormalContainerExit bool                   `yaml:"ignore_abnormal_container_exit"`
}

func (s Design) IsValid(b []byte) error {
	e := util.StringErrorFunc("invalid Design")

	if err := s.NodeDesigns.IsValid(b); err != nil {
		return e(err, "")
	}

	if len(s.Expects) < 1 {
		return e(nil, "empty expects")
	}

	for i := range s.Expects {
		if err := s.Expects[i].IsValid(b); err != nil {
			return e(err, "")
		}
	}

	if _, found := util.CheckSliceDuplicated(s.Nodes.SameHost, func(i interface{}, _ int) string {
		return i.(string) //nolint:forcetypeassert //...
	}); found {
		return e(nil, "duplicated nodes found")
	}

	return nil
}

type NodeDesigns struct {
	Common      string            `yaml:"common"`
	NumberNodes *int              `yaml:"number-nodes"`
	Nodes       map[string]string `yaml:"nodes"`
	Genesis     string            `yaml:"genesis"`
}

func (s NodeDesigns) IsValid(b []byte) error {
	e := util.StringErrorFunc("invalid NodeDesigns")

	if (s.NumberNodes == nil || *s.NumberNodes < 1) && len(s.Nodes) < 1 {
		return e(nil, "empty nodes")
	}

	if len(s.Nodes) > 0 {
		// NOTE check node alias format
		for i := range s.Nodes {
			if err := isValidNodeAliasFormat(i); err != nil {
				return e(err, "")
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

type ExpectScenario struct {
	Condition string                `yaml:"condition"`
	Range     []map[string][]string `yaml:"range"`
	Actions   []ScenarioAction      `yaml:"actions"`
	Registers []ScenarioRegister    `yaml:"registers"`
}

func (s ExpectScenario) IsValid(b []byte) error {
	e := util.StringErrorFunc("invalid ExpectScenario")

	if len(s.Condition) < 1 {
		return e(nil, "empty condition")
	}

	for i := range s.Actions {
		if err := s.Actions[i].IsValid(b); err != nil {
			return e(err, "")
		}
	}

	for i := range s.Registers {
		if err := s.Registers[i].IsValid(b); err != nil {
			return e(err, "")
		}
	}

	return nil
}

func (s ExpectScenario) RangeValues() []map[string]interface{} {
	if len(s.Range) < 1 {
		return nil
	}

	var l int

	for i := range s.Range {
		for k := range s.Range[i] {
			l = len(s.Range[i][k])

			break
		}

		break //lint:ignore SA4004 //...
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
	newexpect.Actions = make([]ScenarioAction, len(s.Actions))

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

type ScenarioAction struct {
	Type  string                `yaml:"type"`
	Args  []string              `yaml:"args"`
	Range []map[string][]string `yaml:"range"`
}

func (s ScenarioAction) IsValid([]byte) error {
	e := util.StringErrorFunc("invalid ScenarioAction")

	switch {
	case len(s.Type) < 1:
		return e(nil, "empty type")
	}

	return nil
}

func (s ScenarioAction) RangeValues() []map[string]interface{} {
	if len(s.Range) < 1 {
		return nil
	}

	var l int

	for i := range s.Range {
		for k := range s.Range[i] {
			l = len(s.Range[i][k])

			break
		}

		break //lint:ignore SA4004 //...
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

type ScenarioRegister struct {
	Type   string `yaml:"type"`
	Assign string `yaml:"assign"`
	Format string `yaml:"format"`
}

func (s ScenarioRegister) IsValid([]byte) error {
	e := util.StringErrorFunc("invalid ScenarioRegister")

	switch {
	case len(s.Assign) < 1:
		return e(nil, "empty assign")
	case !strings.HasPrefix(s.Assign, "."):
		return e(nil, "wrong assign format; must start with `.`")
	case strings.HasSuffix(s.Assign, "."):
		return e(nil, "wrong assign format; must not end with `.`")
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
	e := util.StringErrorFunc("invalid node alias")

	s = strings.TrimSpace(s)

	switch {
	case len(s) < 1:
		return e(nil, "empty alias")
	case !reNodeAlias.MatchString(s):
		return e(nil, "wrong format")
	default:
		return nil
	}
}

func nodeAlias(i int) string {
	return fmt.Sprintf("no%d", i)
}
