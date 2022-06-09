package contest

import (
	"bufio"
	"bytes"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"text/template"

	"github.com/pkg/errors"
	"github.com/spikeekips/mitum/base"
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
	sync.RWMutex
	m       map[string]interface{}
	funcMap template.FuncMap
}

func NewVars(m map[string]interface{}) *Vars {
	if m == nil {
		m = map[string]interface{}{}
	}

	vs := &Vars{
		m:       m,
		funcMap: template.FuncMap{},
	}

	return vs
}

func (vs *Vars) Clone(m map[string]interface{}) *Vars {
	vars := func() *Vars {
		vs.RLock()
		defer vs.RUnlock()

		nvars := NewVars(copyValue(reflect.ValueOf(vs.m)).Interface().(map[string]interface{}))
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

	base := vs.baseFuncMap()
	for k := range base {
		m[k] = base[k]
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

func SanitizeVarsMap(m interface{}) interface{} {
	if m == nil {
		return m
	}

	v := reflect.ValueOf(m)
	switch v.Kind() {
	case reflect.Map:
		n := reflect.MakeMapWithSize(
			reflect.MapOf(reflect.TypeOf((*string)(nil)).Elem(), reflect.TypeOf((*interface{})(nil)).Elem()),
			0,
		)

		vr := v.MapRange()
		for vr.Next() {
			n.SetMapIndex(
				reflect.ValueOf(NormalizeVarsKey(vr.Key().Interface().(string))),
				reflect.ValueOf(SanitizeVarsMap(vr.Value().Interface())),
			)
		}

		v = n
	case reflect.Array, reflect.Slice:
		n := reflect.MakeSlice(reflect.SliceOf(reflect.TypeOf((*interface{})(nil)).Elem()), 0, v.Len())

		for i := 0; i < v.Len(); i++ {
			n = reflect.Append(n, reflect.ValueOf(SanitizeVarsMap(v.Index(i).Interface())))
		}
		v = n
	}

	return v.Interface()
}

func NormalizeVarsKey(s string) string {
	s = strings.TrimSpace(s)

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
		strings.Title,
		func(i string) string { // NOTE remove blank
			return strings.ReplaceAll(i, " ", "")
		},
	} {
		s = f(s)
	}

	return s
}

func getVar(v interface{}, keys string) (interface{}, bool) {
	if strings.Contains(keys, "no0") {
		panic("showme")
	}

	ks := strings.Split(keys, ".")[1:]

	m := v
	for _, k := range ks[1:] {
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
			l[k] = map[string]interface{}{}
			l = l[k].(map[string]interface{})
		} else {
			l = j.(map[string]interface{})
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
			l[k] = map[string]interface{}{}
			l = l[k].(map[string]interface{})
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
			vs.Lock()
			defer vs.Unlock()

			return base.NewMPrivatekey()
		},
	}
}

func copyValue(v reflect.Value) reflect.Value {
	v = reflect.ValueOf(v.Interface())
	switch v.Kind() {
	case reflect.Map:
		n := reflect.ValueOf(map[string]interface{}{})
		vr := v.MapRange()
		for vr.Next() {
			setMapIndexStringKey(n, vr.Key(), copyValue(vr.Value()))
		}

		return n
	case reflect.Slice:
		n := reflect.MakeSlice(reflect.SliceOf(reflect.TypeOf(v.Interface()).Elem()), 0, v.Len())
		for i := 0; i < v.Len(); i++ {
			n = reflect.Append(n, copyValue(v.Index(i)))
		}

		return n
	default:
		return v
	}
}

func setMapIndexStringKey(m, k, v reflect.Value) {
	key := k.Interface().(string)
	m.SetMapIndex(reflect.ValueOf(key), v)
}

func CompileTemplate(s string, vars *Vars, extra map[string]interface{}) (string, error) {
	t, err := template.New("s").Funcs(vars.FuncMap()).Parse(s)
	if err != nil {
		return "", err
	}

	v := vars.Map()
	for i := range extra {
		v[i] = extra[i]
	}

	var bf bytes.Buffer
	if err := t.Execute(&bf, v); err != nil {
		return "", err
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
		return "", err
	}

	return bf.String(), nil
}
