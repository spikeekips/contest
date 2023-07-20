package main

import (
	"context"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"syscall"

	"github.com/arl/statsviz"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/spikeekips/mitum/base"
	"github.com/spikeekips/mitum/isaac"
	isaacstates "github.com/spikeekips/mitum/isaac/states"
	"github.com/spikeekips/mitum/launch"
	"github.com/spikeekips/mitum/network/quicstream"
	"github.com/spikeekips/mitum/util"
	"github.com/spikeekips/mitum/util/logging"
	"github.com/spikeekips/mitum/util/ps"
)

type DebugFlags struct {
	DebugHTTP string `name:"debug-http" help:"runtime debug thru https" group:"dev" placeholder:"bind address" default:":9090"`
	Statsviz  bool   `name:"statsviz" help:"enable statsviz thru https" group:"dev"`
	Pprof     bool   `name:"pprof" help:"enable runtime pprof thru https" group:"dev"`
}

type RunCommand struct { //nolint:govet //...
	//revive:disable:line-length-limit
	launch.DesignFlag
	launch.DevFlags `embed:"" prefix:"dev."`
	DebugFlags      `embed:"" prefix:"dev."`
	Vault           string                `name:"vault" help:"privatekey path of vault"`
	Discovery       []launch.ConnInfoFlag `help:"member discovery" placeholder:"connection info"`
	Hold            launch.HeightFlag     `help:"hold consensus states" placeholder:"height"`
	DryRun          bool                  `name:"dry-run" help:"check design and exit"`
	exitf           func(error)
	log             *zerolog.Logger
	holded          bool
	mux             *http.ServeMux
	//revive:enable:line-length-limit
}

func (cmd *RunCommand) Run(pctx context.Context) error {
	var log *logging.Logging
	if err := util.LoadFromContextOK(pctx, launch.LoggingContextKey, &log); err != nil {
		return err
	}

	log.Log().Debug().
		Interface("design", cmd.DesignFlag).
		Interface("vault", cmd.Vault).
		Interface("discovery", cmd.Discovery).
		Interface("hold", cmd.Hold).
		Interface("debug_http", cmd.DebugHTTP).
		Interface("statsviz", cmd.Statsviz).
		Interface("pprof", cmd.Pprof).
		Interface("dev", cmd.DevFlags).
		Msg("flags")

	cmd.log = log.Log()

	if cmd.Statsviz {
		if err := cmd.enableStatsviz(); err != nil {
			return errors.Wrap(err, "enable statsviz")
		}
	}

	if cmd.Pprof {
		if err := cmd.enablePprof(); err != nil {
			return errors.Wrap(err, "enable pprof")
		}
	}

	if cmd.mux != nil {
		addr, err := net.ResolveTCPAddr("tcp", cmd.DebugHTTP)
		if err != nil {
			return errors.Wrap(err, "parse --debug-http")
		}

		go func() {
			_ = http.ListenAndServe(addr.String(), cmd.mux)
		}()
	}

	//revive:disable:modifies-parameter
	pctx = context.WithValue(pctx, launch.DesignFlagContextKey, cmd.DesignFlag)
	pctx = context.WithValue(pctx, launch.DevFlagsContextKey, cmd.DevFlags)
	pctx = context.WithValue(pctx, launch.DiscoveryFlagContextKey, cmd.Discovery)
	pctx = context.WithValue(pctx, launch.VaultContextKey, cmd.Vault)
	//revive:enable:modifies-parameter

	pps := launch.DefaultRunPS()

	if cmd.DryRun {
		_ = pps.POK(launch.PNameDesign).
			PostAddOK("exit", func(pctx context.Context) (context.Context, error) {
				log.Log().Debug().Msg("design ok")

				return pctx, ps.ErrIgnoreLeft.WithStack()
			})
	}

	_ = pps.POK(launch.PNameStorage).PostAddOK(ps.Name("check-hold"), cmd.pCheckHold)
	_ = pps.POK(launch.PNameStates).PreAddOK(
		ps.Name("when-new-block-saved-in-consensus-state-func"), cmd.pWhenNewBlockSavedInConsensusStateFunc)
	_ = pps.POK(launch.PNameStates).
		PreAfterOK(ps.Name("custom-proposer-selector"), PProposerSelector, launch.PNameProposerSelector).
		PreBeforeOK(PNameFilterNotifyMsgFunc, PFilterNotifyMsgFunc, launch.PNameNetworkHandlers).
		PreAfterOK(PNameCustomBallotStuckResolver, PBallotStuckResolver, launch.PNameBallotStuckResolver)

	_ = pps.SetLogging(log)

	log.Log().Debug().Interface("process", pps.Verbose()).Msg("process ready")

	pctx, err := pps.Run(pctx) //revive:disable-line:modifies-parameter
	defer func() {
		log.Log().Debug().Interface("process", pps.Verbose()).Msg("process will be closed")

		if _, err = pps.Close(pctx); err != nil {
			log.Log().Error().Err(err).Msg("failed to close")
		}
	}()

	if err != nil || cmd.DryRun {
		return err
	}

	log.Log().Debug().
		Interface("discovery", cmd.Discovery).
		Interface("hold", cmd.Hold.Height()).
		Msg("node started")

	return cmd.run(pctx)
}

var errHoldStop = util.NewIDError("hold stop")

func (cmd *RunCommand) run(pctx context.Context) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	exitch := make(chan error)

	cmd.exitf = func(err error) {
		exitch <- err
	}

	stopstates := func() {}

	if !cmd.holded {
		deferred, err := cmd.runStates(ctx, pctx)
		if err != nil {
			return err
		}

		stopstates = deferred
	}

	select {
	case <-ctx.Done(): // NOTE graceful stop
		cmd.log.Debug().Msg("stopped by signal")

		return nil
	case err := <-exitch:
		if errors.Is(err, errHoldStop) {
			stopstates()

			<-ctx.Done()

			cmd.log.Debug().Msg("stopped by signal")

			return nil
		}

		return err
	}
}

func (cmd *RunCommand) runStates(ctx, pctx context.Context) (func(), error) {
	var discoveries *util.Locked[[]quicstream.ConnInfo]
	var states *isaacstates.States

	if err := util.LoadFromContextOK(pctx,
		launch.DiscoveryContextKey, &discoveries,
		launch.StatesContextKey, &states,
	); err != nil {
		return nil, err
	}

	if dis := launch.GetDiscoveriesFromLocked(discoveries); len(dis) < 1 {
		cmd.log.Warn().Msg("empty discoveries; will wait to be joined by remote nodes")
	}

	go func() {
		cmd.exitf(<-states.Wait(ctx))
	}()

	return func() {
		if err := states.Hold(); err != nil && !errors.Is(err, util.ErrDaemonAlreadyStopped) {
			cmd.log.Error().Err(err).Msg("failed to stop states")

			return
		}

		cmd.log.Debug().Msg("states stopped")
	}, nil
}

func (cmd *RunCommand) pWhenNewBlockSavedInConsensusStateFunc(pctx context.Context) (context.Context, error) {
	var log *logging.Logging

	if err := util.LoadFromContextOK(pctx,
		launch.LoggingContextKey, &log,
	); err != nil {
		return pctx, err
	}

	//revive:disable-next-line:modifies-parameter
	pctx = context.WithValue(pctx,
		launch.WhenNewBlockSavedInConsensusStateFuncContextKey,
		func(m base.BlockMap) {
			l := log.Log().With().Interface("blockmap", m).Logger()

			if cmd.Hold.IsSet() && m.Manifest().Height() == cmd.Hold.Height() {
				l.Debug().Msg("will be stopped by hold")

				cmd.exitf(errHoldStop.WithStack())

				return
			}
		},
	)

	return pctx, nil
}

func (cmd *RunCommand) pCheckHold(pctx context.Context) (context.Context, error) {
	var db isaac.Database
	if err := util.LoadFromContextOK(pctx, launch.CenterDatabaseContextKey, &db); err != nil {
		return pctx, err
	}

	switch {
	case !cmd.Hold.IsSet():
	case cmd.Hold.Height() < base.GenesisHeight:
		cmd.holded = true
	default:
		switch m, found, err := db.LastBlockMap(); {
		case err != nil:
			return pctx, err
		case !found:
		case cmd.Hold.Height() <= m.Manifest().Height():
			cmd.holded = true
		}
	}

	return pctx, nil
}

func (cmd *RunCommand) enableStatsviz() error {
	if cmd.mux == nil {
		cmd.mux = http.NewServeMux()
	}

	if err := statsviz.Register(cmd.mux); err != nil {
		return errors.Wrap(err, "register statsviz for http-state")
	}

	cmd.log.Debug().Msg("statsviz registered")

	return nil
}

func (cmd *RunCommand) enablePprof() error {
	cmd.log.Debug().Msg("pprof registered")

	if cmd.mux == nil {
		cmd.mux = http.NewServeMux()
	}

	cmd.mux.HandleFunc("/debug/pprof/", pprof.Index)
	cmd.mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	cmd.mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	cmd.mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	cmd.mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	return nil
}
