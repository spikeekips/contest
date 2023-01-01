package main

import (
	"context"
	"sync"

	"github.com/dop251/goja"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/spikeekips/mitum/launch"
	"github.com/spikeekips/mitum/util"
	"github.com/spikeekips/mitum/util/logging"
	"github.com/spikeekips/mitum/util/ps"
)

var PNameFilterNotifyMsgFunc = ps.Name("filter-notify-msg-func")

func PFilterNotifyMsgFunc(ctx context.Context) (context.Context, error) {
	var log *logging.Logging
	var designString string
	var oldfilternotifymsg launch.FilterMemberlistNotifyMsgFunc

	if err := util.LoadFromContextOK(ctx,
		launch.LoggingContextKey, &log,
		launch.DesignStringContextKey, &designString,
		launch.FilterMemberlistNotifyMsgFuncContextKey, &oldfilternotifymsg,
	); err != nil {
		return ctx, err
	}

	var script string

	switch i, err := loadScript(designString, "filter-notify-msg-func"); {
	case errors.Is(err, util.ErrNotFound):
		return ctx, nil
	case err != nil:
		return ctx, err
	default:
		script = i
	}

	mlog := logging.NewLogging(func(zctx zerolog.Context) zerolog.Context {
		return zctx.Str("module", "filter-notify-msg")
	}).SetLogging(log)

	vm, err := prepareJAVM(mlog)
	if err != nil {
		return ctx, err
	}

	if _, err := vm.RunString(script); err != nil {
		return ctx, errors.WithStack(err)
	}

	caller, ok := goja.AssertFunction(vm.Get("filterNotifyMsg"))
	if !ok {
		return ctx, errors.Errorf("function, `filterNotifyMsg` not found in `filter-notify-msg-func` design")
	}

	log.Log().Debug().Str("script", script).Msg("`filterNotifyMsg` loaded from design")

	return context.WithValue(ctx, launch.FilterMemberlistNotifyMsgFuncContextKey,
		launch.FilterMemberlistNotifyMsgFunc(filterNotifyMsgFunc(vm, caller, oldfilternotifymsg)),
	), nil
}

func filterNotifyMsgFunc(vm *goja.Runtime, f goja.Callable, old launch.FilterMemberlistNotifyMsgFunc) launch.FilterMemberlistNotifyMsgFunc {
	var lock sync.Mutex

	return func(i interface{}) bool {
		lock.Lock()
		defer lock.Unlock()

		b, err := util.MarshalJSON(i)
		if err != nil {
			return true
		}

		var m map[string]interface{}

		if err := util.UnmarshalJSON(b, &m); err != nil {
			return true
		}

		res, err := f(goja.Undefined(), vm.ToValue(m))
		if err != nil {
			return true
		}

		switch t := res.Export().(type) {
		case bool:
			if !t {
				return false
			}

			return old(i)
		default:
			return true // NOTE ignore
		}
	}
}
