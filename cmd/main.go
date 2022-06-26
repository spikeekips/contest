package main

import (
	"github.com/alecthomas/kong"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/spikeekips/mitum/launch"
	_ "github.com/spikeekips/mitum/launch"
	mitumlogging "github.com/spikeekips/mitum/util/logging"
)

var (
	logging *mitumlogging.Logging
	log     *zerolog.Logger
)

var kongOptions = []kong.Option{
	kong.Name("contest"),
	kong.Vars{
		"mongodb_uri": defaultMongodbURI,
	},
}

func main() {
	var cli struct {
		launch.Logging `embed:"" prefix:"log."`
		Run            runCommand `cmd:"" help:"run contest"`
	}

	kctx := kong.Parse(&cli, kongOptions...)

	l, err := launch.SetupLoggingFromFlags(cli.Logging)
	if err != nil {
		kctx.FatalIfErrorf(err)
	}
	logging = l

	log = mitumlogging.NewLogging(func(lctx zerolog.Context) zerolog.Context {
		return lctx.Str("module", "main")
	}).SetLogging(logging).Log()

	log.Debug().Str("command", kctx.Command()).Msg("start command")

	if err := func() error {
		defer log.Info().Msg("stopped")

		return errors.WithStack(kctx.Run())
	}(); err != nil {
		log.Error().Err(err).Msg("stopped by error")

		kctx.FatalIfErrorf(err)
	}
}
