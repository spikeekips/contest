package main

import (
	"context"

	"github.com/pkg/errors"
	"github.com/spikeekips/mitum/base"
	"github.com/spikeekips/mitum/launch"
	"github.com/spikeekips/mitum/util"
	"github.com/spikeekips/mitum/util/logging"
	"github.com/spikeekips/mitum/util/ps"
)

var PNameCustomBallotStuckResolver = ps.Name("custom-ballot-stuck-resolver")

type DummyBallotStuckResolver struct {
	vpch <-chan base.Voteproof
}

func NewDummyBallotStuckResolver() *DummyBallotStuckResolver {
	return &DummyBallotStuckResolver{vpch: make(chan base.Voteproof)}
}

func (*DummyBallotStuckResolver) NewPoint(context.Context, base.StagePoint) bool {
	return false
}

func (r *DummyBallotStuckResolver) Voteproof() <-chan base.Voteproof {
	return r.vpch
}

func (r *DummyBallotStuckResolver) Clean() {
	return
}

func (r *DummyBallotStuckResolver) Cancel(base.StagePoint) {
	return
}

func PBallotStuckResolver(ctx context.Context) (context.Context, error) {
	var log *logging.Logging
	var designString string

	if err := util.LoadFromContextOK(ctx,
		launch.LoggingContextKey, &log,
		launch.DesignStringContextKey, &designString,
	); err != nil {
		return ctx, err
	}

	var name string

	switch err := loadConfigFromDesign(designString, "ballot-stuck-resolver", &name); {
	case errors.Is(err, util.ErrNotFound):
		log.Log().Debug().Msg("default ballot stuck resolver will be used")

		return ctx, nil
	case err != nil:
		return ctx, err
	}

	switch name {
	case "dummy":
		log.Log().Debug().Msg("dummy ballot stuck resolver loaded and will be used")

		return context.WithValue(ctx, launch.BallotStuckResolverContextKey, NewDummyBallotStuckResolver()), nil
	case "default", "":
		log.Log().Debug().Msg("default ballot stuck resolver will be used")

		return ctx, nil
	default:
		return ctx, errors.Errorf("unknown ballot stuck resolver name, %q", name)
	}
}
