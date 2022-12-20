package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kong"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/spikeekips/mitum/launch"
	"github.com/spikeekips/mitum/util"
	mitumlogging "github.com/spikeekips/mitum/util/logging"
)

var (
	Version   = "v0.0.1"
	BuildTime = "-"
	GitBranch = "-"
	GitCommit = "-"
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
	var cli struct {
		launch.LoggingFlags `embed:"" prefix:"log."`
		Run                 runCommand `cmd:"" help:"run contest"`
		Version             struct{}   `cmd:"" help:"version"`
	}

	kctx := kong.Parse(&cli, kongOptions...)

	bi, err := util.ParseBuildInfo(Version, GitBranch, GitCommit, BuildTime)
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
