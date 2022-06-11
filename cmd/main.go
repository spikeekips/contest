package main

import (
	"os"

	"github.com/alecthomas/kong"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
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
	logging = mitumlogging.Setup(os.Stderr, zerolog.DebugLevel, "json", false)
	log = mitumlogging.NewLogging(func(lctx zerolog.Context) zerolog.Context {
		return lctx.Str("module", "main")
	}).SetLogging(logging).Log()

	var cli struct {
		Run runCommand `cmd:"" help:"run contest"`
	}

	kctx := kong.Parse(&cli, kongOptions...)

	log.Info().Str("command", kctx.Command()).Msg("start command")

	if err := func() error {
		defer log.Info().Msg("stopped")

		return errors.Wrap(kctx.Run(), "")
	}(); err != nil {
		log.Error().Err(err).Msg("stopped by error")

		kctx.FatalIfErrorf(err)
	}
}
