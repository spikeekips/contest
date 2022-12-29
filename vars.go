package contest

import (
	"bufio"
	"bytes"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"text/template"

	"github.com/pkg/errors"
	"github.com/spikeekips/mitum/base"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

var (
	reFilterSymbol = regexp.MustCompile(`[\-\_\.]`)
	reFilterBlank  = regexp.MustCompile(`[\s][\s]*`)
)

var TranslateWords = map[string]string{
	"ssh": "SSH",
	"id":  "ID",
	"url": "URL",
	"uri": "URI",
}

type Vars struct {
	m       map[string]interface{}
	funcMap template.FuncMap
	sync.RWMutex
}

func NewVars(m map[string]interface{}) *Vars {
	i := m

	if i == nil {
		i = map[string]interface{}{}
	}

	vs := &Vars{
		m:       i,
		funcMap: template.FuncMap{},
	}

	return vs
}

func (vs *Vars) Clone(m map[string]interface{}) *Vars {
	vars := func() *Vars {
		vs.RLock()
		defer vs.RUnlock()

		nvars := NewVars(copyValue(reflect.ValueOf(vs.m)). //nolint:forcetypeassert //...
									Interface().(map[string]interface{}))
		nvars.funcMap = vs.funcMap

		return nvars
	}()

	for k := range m {
		vars.Set(k, m[k])
	}

	return vars
}

func (vs *Vars) FuncMap() template.FuncMap {
	vs.RLock()
	defer vs.RUnlock()

	m := template.FuncMap{}
	for k := range vs.funcMap {
		m[k] = vs.funcMap[k]
	}

	b := vs.baseFuncMap()
	for k := range b {
		m[k] = b[k]
	}

	return m
}

func (vs *Vars) AddFunc(name string, f interface{}) *Vars {
	vs.Lock()
	defer vs.Unlock()

	vs.funcMap[name] = f

	return vs
}

func (vs *Vars) Map() map[string]interface{} {
	vs.RLock()
	defer vs.RUnlock()

	return vs.m
}

func (vs *Vars) Exists(keys string) bool {
	vs.RLock()
	defer vs.RUnlock()

	_, found := getVar(vs.m, keys)

	return found
}

func (vs *Vars) Value(keys string) (interface{}, bool) {
	vs.RLock()
	defer vs.RUnlock()

	return getVar(vs.m, keys)
}

func (vs *Vars) Set(keys string, value interface{}) {
	vs.Lock()
	defer vs.Unlock()

	err := setVar(vs.m, keys, value)
	if err != nil {
		panic(err)
	}
}

func (vs *Vars) Rename(keys, newkeys string) {
	vs.Lock()
	defer vs.Unlock()

	err := renameVar(vs.m, keys, newkeys)
	if err != nil {
		panic(err)
	}
}

func NormalizeVarsKey(s string) string {
	k := strings.TrimSpace(s)

	for _, f := range []func(i string) string{
		func(i string) string { // NOTE replace hyphen and underscore
			return string(reFilterSymbol.ReplaceAll([]byte(i), []byte(" ")))
		},
		func(i string) string { // NOTE replace 2 word into capitals
			a := ""
			for _, w := range reFilterBlank.Split(i, -1) {
				if x, found := TranslateWords[strings.ToLower(w)]; found {
					w = x
				}

				a += " " + w
			}

			return a
		},
		func(i string) string {
			return cases.Title(language.Und).String(i)
		},
		func(i string) string { // NOTE remove blank
			return strings.ReplaceAll(i, " ", "")
		},
	} {
		k = f(k)
	}

	return k
}

func getVar(v interface{}, keys string) (interface{}, bool) {
	ks := strings.Split(keys, ".")[1:]

	m := v
	for _, k := range ks {
		if i, ok := m.(map[string]interface{}); !ok {
			return nil, false
		} else if j, found := i[k]; !found {
			return nil, false
		} else {
			m = j
		}
	}

	return m, true
}

func setVar(m map[string]interface{}, keys string, v interface{}) error {
	switch {
	case m == nil:
		return errors.Errorf("nil map")
	case !strings.HasPrefix(keys, "."):
		return errors.Errorf("wrong key format; must start with `.`")
	}

	ks := strings.Split(keys, ".")[1:]

	l := m
	for _, k := range ks[:len(ks)-1] {
		if j, found := l[k]; !found {
			l[k] = map[string]interface{}{}   //nolint:forcetypeassert //...
			l = l[k].(map[string]interface{}) //nolint:forcetypeassert //...
		} else {
			l = j.(map[string]interface{}) //nolint:forcetypeassert //...
		}
	}

	l[ks[len(ks)-1]] = v

	return nil
}

func renameVar(m map[string]interface{}, keys, newkey string) error {
	switch {
	case m == nil:
		return errors.Errorf("nil map")
	case !strings.HasPrefix(keys, "."):
		return errors.Errorf("wrong key format; must start with `.`")
	}

	ks := strings.Split(keys, ".")[1:]

	var v interface{}
	var foundkey bool

	l := m

	for _, k := range ks {
		if j, found := l[k]; !found {
			l[k] = map[string]interface{}{}   //nolint:forcetypeassert //...
			l = l[k].(map[string]interface{}) //nolint:forcetypeassert //...
		} else {
			v = j
			foundkey = true

			delete(l, k)

			break
		}
	}

	if !foundkey {
		return errors.Errorf("key not found, %q", keys)
	}

	return setVar(m, newkey, v)
}

func (vs *Vars) baseFuncMap() template.FuncMap {
	return template.FuncMap{
		"existsVar": func(keys string) interface{} {
			vs.RLock()
			defer vs.RUnlock()

			_, found := getVar(vs.m, keys)

			return found
		},
		"getVar": func(keys string) interface{} {
			vs.RLock()
			defer vs.RUnlock()

			i, _ := getVar(vs.m, keys)

			return i
		},
		"setVar": func(keys string, value interface{}) string {
			vs.Lock()
			defer vs.Unlock()

			_ = setVar(vs.m, keys, value)

			return ""
		},
		"setgetVar": func(keys string, value interface{}) interface{} {
			vs.Lock()
			defer vs.Unlock()

			_ = setVar(vs.m, keys, value)

			return value
		},
		"newKey": func() base.Privatekey {
			return base.NewMPrivatekey()
		},
		"addInt": func(a interface{}, b int) int64 {
			i, err := strconv.ParseInt(fmt.Sprintf("%v", a), 10, 64)
			if err != nil {
				return 0
			}

			return i + int64(b)
		},
	}
}

func copyValue(v reflect.Value) reflect.Value {
	j := reflect.ValueOf(v.Interface())
	switch j.Kind() {
	case reflect.Map:
		n := reflect.ValueOf(map[string]interface{}{})

		vr := j.MapRange()
		for vr.Next() {
			setMapIndexStringKey(n, vr.Key(), copyValue(vr.Value()))
		}

		return n
	case reflect.Slice:
		n := reflect.MakeSlice(reflect.SliceOf(reflect.TypeOf(j.Interface()).Elem()), 0, j.Len())
		for i := 0; i < j.Len(); i++ {
			n = reflect.Append(n, copyValue(j.Index(i)))
		}

		return n
	default:
		return j
	}
}

func setMapIndexStringKey(m, k, v reflect.Value) {
	key := k.Interface().(string) //nolint:forcetypeassert //...
	m.SetMapIndex(reflect.ValueOf(key), v)
}

func CompileTemplate(s string, vars *Vars, extra map[string]interface{}) (string, error) {
	t, err := template.New("s").Funcs(vars.FuncMap()).Parse(s)
	if err != nil {
		return "", errors.WithStack(err)
	}

	v := vars.Map()
	for i := range extra {
		v[i] = extra[i]
	}

	var bf bytes.Buffer
	if err := t.Execute(&bf, v); err != nil {
		return "", errors.WithStack(err)
	}

	sc := bufio.NewScanner(bytes.NewReader(bf.Bytes()))
	var ln int

	for sc.Scan() {
		l := sc.Text()
		if strings.Contains(l, "<no value>") {
			return "", errors.Errorf("some variables are not replaced in template string, %q(line: %d)", l, ln)
		}
		ln++
	}

	if err := sc.Err(); err != nil {
		return "", errors.WithStack(err)
	}

	return bf.String(), nil
}
