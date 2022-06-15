package main

import (
	"os"

	"github.com/alecthomas/kong"
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
	var cli struct {
		Logging struct {
			Level      LogLevel `name:"level" default:"debug" help:"log level: {${enum}}" group:"logging"`
			Type       string   `enum:"json, terminal" default:"terminal" help:"log format: {${enum}}" group:"logging"`
			Out        string   `enum:"stdout, stderr, <file>" default:"stderr" help:"log output file: {${enum}}" group:"logging"`
			ForceColor bool     `name:"force-color" negatable:"" help:"log force color" group:"logging"`
		} `embed:"" prefix:"log."`
		Run runCommand `cmd:"" help:"run contest"`
	}

	kctx := kong.Parse(&cli, kongOptions...)

	logging = mitumlogging.Setup(os.Stderr, cli.Logging.Level.level, cli.Logging.Type, cli.Logging.ForceColor)
	log = mitumlogging.NewLogging(func(lctx zerolog.Context) zerolog.Context {
		return lctx.Str("module", "main")
	}).SetLogging(logging).Log()

	log.Debug().Str("command", kctx.Command()).Msg("start command")

	if err := func() error {
		defer log.Info().Msg("stopped")

		return kctx.Run()
	}(); err != nil {
		log.Error().Err(err).Msg("stopped by error")

		kctx.FatalIfErrorf(err)
	}
}
