package main

import (
	"flag"
	"os"
	"time"

	resttunnel "github.com/TheRockettek/RestTunnel/internal"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

func main() {
	var lFlag = flag.String("level", "info", "Log level to use (debug/info/warn/error/fatal/panic/no/disabled/trace)")
	flag.Parse()

	level, err := zerolog.ParseLevel(*lFlag)

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
		log.Fatal().Err(err).Send()
	}

	log.Info().Msg("Listening on tunnel")
	err = fasthttp.ListenAndServe("0.0.0.0:8000", restTunnel.HandleRequest)
	if err != nil {
		log.Fatal().Err(err).Send()
	}
}
