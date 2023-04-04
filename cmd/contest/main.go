package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kong"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	contest "github.com/spikeekips/contest2"
	"github.com/spikeekips/mitum/launch"
	mitumlogging "github.com/spikeekips/mitum/util/logging"
	_ "go.uber.org/automaxprocs"
)

var (
	Version        = "v0.0.1"
	BuildTime      = "-"
	GitBranch      = "-"
	GitCommit      = "-"
	MitumVersion   = "v0.0.1"
	MitumGitBranch = "-"
	MitumGitCommit = "-"
)

var (
	mlogging *mitumlogging.Logging
	log      *zerolog.Logger
)

var kongOptions = []kong.Option{
	kong.Name("contest"),
	kong.Vars{
		"mongodb_uri":     defaultMongodbURI,
		"log_out":         "stderr",
		"log_format":      "terminal",
		"log_level":       "debug",
		"log_force_color": "false",
	},
}

func main() {
	var cli struct { //nolint:govet //...
		//revive:disable:nested-structs
		launch.LoggingFlags `embed:"" prefix:"log."`
		Run                 runCommand `cmd:"" help:"run contest"`
		Version             struct{}   `cmd:"" help:"version"`
		//revive:enable:nested-structs
	}

	kctx := kong.Parse(&cli, kongOptions...)

	bi, err := contest.ParseBuildInfo(
		Version, GitBranch, GitCommit,
		MitumVersion, MitumGitBranch, MitumGitCommit,
		BuildTime,
	)
	if err != nil {
		kctx.FatalIfErrorf(err)
	}

	if kctx.Command() == "version" {
		_, _ = fmt.Fprintln(os.Stdout, bi.String())

		return
	}

	l, err := launch.SetupLoggingFromFlags(cli.LoggingFlags)
	if err != nil {
		kctx.FatalIfErrorf(err)
	}

	mlogging = l

	log = mitumlogging.NewLogging(func(lctx zerolog.Context) zerolog.Context {
		return lctx.Str("module", "main")
	}).SetLogging(mlogging).Log()

	log.Debug().Str("command", kctx.Command()).Msg("start command")

	if err := func() error {
		defer log.Info().Msg("stopped")

		return errors.WithStack(kctx.Run())
	}(); err != nil {
		log.Error().Err(err).Msg("stopped by error")

		kctx.FatalIfErrorf(err)
	}
}
