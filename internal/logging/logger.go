// Package logging configures zerolog the way Portainer does
// (portainer server-ce/api/logs).
package logging

import (
	"fmt"
	stdlog "log"
	"os"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Configure sets the global logger's level and format. Format "json" writes
// one JSON object per line; anything else writes Portainer's console format:
// TIMESTAMP LEVEL LOC > MESSAGE | context=... field=value.
func Configure(level, format string) error {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil || lvl == zerolog.NoLevel {
		return fmt.Errorf("invalid log level %q", level)
	}
	zerolog.SetGlobalLevel(lvl)

	logger := zerolog.New(os.Stderr)
	if format != "json" {
		logger = zerolog.New(zerolog.ConsoleWriter{
			Out:           os.Stderr,
			TimeFormat:    "2006/01/02 03:04PM",
			FormatMessage: formatMessage,
		})
	}
	log.Logger = logger.With().Timestamp().Caller().Logger()

	// Route the standard library's log package through zerolog too.
	stdlog.SetFlags(0)
	stdlog.SetOutput(log.Logger)
	return nil
}

func formatMessage(i any) string {
	if i == nil {
		return ""
	}
	return fmt.Sprintf("%s |", i)
}
