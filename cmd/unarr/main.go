package main

import (
	"github.com/torrentclaw/torrentclaw-cli/internal/cmd"
	"github.com/torrentclaw/torrentclaw-cli/internal/sentry"
)

func main() {
	sentry.Init(cmd.Version)
	defer sentry.Close()
	defer sentry.RecoverPanic()

	cmd.Execute()
}
