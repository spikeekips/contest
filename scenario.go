package contest

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/pkg/errors"
	"github.com/spikeekips/mitum/util"
)

type Design struct {
	Vars                            map[string]interface{} `yaml:"vars"`
	NodeDesigns                     NodeDesigns            `yaml:"designs"`
	Expects                         []ExpectScenario       `yaml:"expects"`
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
	Condition string             `yaml:"condition"`
	Actions   []ScenarioAction   `yaml:"actions"`
	Registers []ScenarioRegister `yaml:"registers"`
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

func (s ExpectScenario) Compile(vars *Vars) (newexpect ExpectScenario, err error) {
	newexpect.Condition, err = CompileTemplate(s.Condition, vars, nil)
	if err != nil {
		return newexpect, errors.Wrap(err, "")
	}

	newexpect.Actions = make([]ScenarioAction, len(s.Actions))
	for i := range s.Actions {
		newexpect.Actions[i], err = s.Actions[i].Compile(vars)
		if err != nil {
			return newexpect, errors.Wrap(err, "")
		}
	}

	newexpect.Registers = make([]ScenarioRegister, len(s.Registers))
	for i := range s.Registers {
		newexpect.Registers[i], err = s.Registers[i].Compile(vars)
		if err != nil {
			return newexpect, errors.Wrap(err, "")
		}
	}

	return newexpect, nil
}

type ScenarioAction struct {
	Type string   `yaml:"type"`
	Args []string `yaml:"args"`
}

func (s ScenarioAction) IsValid([]byte) error {
	e := util.StringErrorFunc("invalid ScenarioAction")

	switch {
	case len(s.Type) < 1:
		return e(nil, "empty type")
	}

	// FIXME check type is known

	return nil
}

func (s ScenarioAction) Compile(vars *Vars) (newaction ScenarioAction, err error) {
	newaction.Type = s.Type

	newaction.Args = make([]string, len(s.Args))
	for i := range s.Args {
		newaction.Args[i], err = CompileTemplate(s.Args[i], vars, nil)
		if err != nil {
			return newaction, errors.Wrap(err, "")
		}
	}

	return newaction, nil
}

type ScenarioRegister struct {
	Type   string `yaml:"type"`
	Assign string `yaml:"assign"`
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

	// FIXME check type is known

	return nil
}

func (s ScenarioRegister) Compile(vars *Vars) (newregister ScenarioRegister, err error) {
	newregister.Type = s.Type

	newregister.Assign, err = CompileTemplate(s.Assign, vars, nil)
	if err != nil {
		return newregister, errors.Wrap(err, "")
	}

	return newregister, nil
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
