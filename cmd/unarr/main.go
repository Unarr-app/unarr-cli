package main

import (
	"github.com/torrentclaw/unarr/internal/cmd"
	"github.com/torrentclaw/unarr/internal/sentry"
)

func main() {
	sentry.Init(cmd.Version)
	defer sentry.Close()
	defer sentry.RecoverPanic()

	cmd.Execute()
}
