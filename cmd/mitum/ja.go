package main

import (
	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/require"
	"github.com/rs/zerolog"
	"github.com/spikeekips/mitum/util/logging"
)

func prepareJAVM(log *logging.Logging) (*goja.Runtime, error) {
	vm := goja.New()
	new(require.Registry).Enable(vm)

	require.RegisterNativeModule("node:log", func(_ *goja.Runtime, module *goja.Object) {
		o := module.Get("exports").(*goja.Object)
		o.Set("debug", printJALog(log.Log().Debug))
		o.Set("info", printJALog(log.Log().Info))
		o.Set("warn", printJALog(log.Log().Warn))
		o.Set("error", printJALog(log.Log().Error))
		o.Set("fatal", printJALog(log.Log().Fatal))
		o.Set("panic", printJALog(log.Log().Panic))
		o.Set("trace", printJALog(log.Log().Trace))
	})
	vm.Set("log", require.Require(vm, "node:log"))

	return vm, nil
}

func printJALog(l func() *zerolog.Event) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		args := call.Arguments

		var msg string

		switch {
		case len(args) < 1:
			return nil
		case len(args)%2 != 1:
			return nil
		default:
			s, ok := args[0].Export().(string)
			if !ok {
				return nil
			}

			msg = s
		}

		e := l()

		for i := 0; i < len(args[1:])/2; i++ {
			j := args[1:][i*2].Export()

			s, ok := j.(string)
			if !ok {
				continue
			}

			e = e.Interface(s, args[1:][(i*2)+1].Export())
		}

		e.Msg(msg)

		return nil
	}
}
