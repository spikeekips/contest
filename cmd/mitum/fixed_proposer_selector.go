package main

import (
	"context"

	"github.com/dop251/goja"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/spikeekips/mitum/base"
	"github.com/spikeekips/mitum/isaac"
	"github.com/spikeekips/mitum/launch"
	"github.com/spikeekips/mitum/util"
	"github.com/spikeekips/mitum/util/logging"
)

type FixedProposerSelector struct {
	proposerSelector isaac.ProposerSelector
	f                func(context.Context, base.Point, []base.Node) (base.Node, error)
}

func (p FixedProposerSelector) Select(ctx context.Context, point base.Point, nodes []base.Node) (base.Node, error) {
	switch n, err := p.f(ctx, point, nodes); {
	case err != nil:
		return nil, err
	case n != nil:
		return n, nil
	default:
		return p.proposerSelector.Select(ctx, point, nodes)
	}
}

func PFixedProposerSelector(ctx context.Context) (context.Context, error) {
	var log *logging.Logging
	var designString string
	var proposerSelector isaac.ProposerSelector

	if err := util.LoadFromContextOK(ctx,
		launch.LoggingContextKey, &log,
		launch.DesignStringContextKey, &designString,
		launch.ProposerSelectorContextKey, &proposerSelector,
	); err != nil {
		return ctx, err
	}

	var script string

	switch i, err := loadScript(designString, "fixed-proposer-selector"); {
	case errors.Is(err, util.ErrNotFound):
		return ctx, nil
	case err != nil:
		return ctx, err
	default:
		script = i
	}

	mlog := logging.NewLogging(func(zctx zerolog.Context) zerolog.Context {
		return zctx.Str("module", "fixed-proposer-selector")
	}).SetLogging(log)

	vm, err := prepareJAVM(mlog)
	if err != nil {
		return ctx, err
	}

	if _, err := vm.RunString(script); err != nil {
		return ctx, errors.WithStack(err)
	}

	caller, ok := goja.AssertFunction(vm.Get("selectProposer"))
	if !ok {
		return ctx, errors.Errorf("function, `selectProposer` not found in `fixed-proposer-selector` design")
	}

	log.Log().Debug().Str("script", script).Msg("`selectProposer` loaded from design")

	p := FixedProposerSelector{proposerSelector: proposerSelector, f: proposerSelectFunc(vm, caller)}

	return context.WithValue(ctx, launch.ProposerSelectorContextKey, p), nil
}

func proposerSelectFunc(vm *goja.Runtime, f goja.Callable) func(context.Context, base.Point, []base.Node) (base.Node, error) {
	return func(_ context.Context, point base.Point, nodes []base.Node) (base.Node, error) {
		jpoint := map[string]interface{}{
			"height": point.Height(),
			"round":  point.Round(),
		}

		jnodes := make([]map[string]interface{}, len(nodes))

		for i := range nodes {
			n := nodes[i]

			jnodes[i] = map[string]interface{}{
				"address":   n.Address().String(),
				"publickey": n.Publickey().String(),
			}
		}

		res, err := f(goja.Undefined(), vm.ToValue(jpoint), vm.ToValue(jnodes))
		if err != nil {
			return nil, err
		}

		switch v := res.Export(); {
		case v == nil:
			return nil, nil
		default:
			s, ok := v.(string)
			if !ok {
				return nil, errors.Errorf("return value, expected string, but %T", v)
			}

			i := util.InSliceFunc(nodes, func(n base.Node) bool {
				return n.Address().String() == s
			})

			if i < 0 {
				return nil, errors.Errorf("unknown node address returned, %q", s)
			}

			return nodes[i], nil
		}
	}
}
