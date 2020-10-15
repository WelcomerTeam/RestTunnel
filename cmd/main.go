package main

import (
	"flag"
	"os"
	"time"

	resttunnel "github.com/TheRockettek/RestTunnel/internal"
	"github.com/rs/zerolog"
)

func main() {
	var lFlag = flag.String("level", "info", "Log level to use (debug/info/warn/error/fatal/panic/no/disabled/trace)")
	flag.Parse()

	level, err := zerolog.ParseLevel(*lFlag)
	if err != nil {
		level = zerolog.InfoLevel
	}

	logger := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: time.Stamp,
	}

	log := zerolog.New(logger).With().Timestamp().Logger()
	if level != zerolog.NoLevel {
		log.Info().Str("logLevel", level.String()).Msg("Using logging")
	}

	zerolog.SetGlobalLevel(level)

	restTunnel, err := resttunnel.NewTunnel(logger)
	if err != nil {
		log.Panic().Err(err).Msgf("Cannot create restTunnel: %s", err)
	}

	err = restTunnel.Open()
	if err != nil {
		log.Panic().Err(err).Msgf("Cannot open restTunnel: %s", err)
	}
}
