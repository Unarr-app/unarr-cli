package main

import (
	"github.com/Unarr-app/unarr-cli/internal/cmd"
	"github.com/Unarr-app/unarr-cli/internal/sentry"
)

func main() {
	sentry.Init(cmd.Version)
	defer sentry.Close()
	defer sentry.RecoverPanic()

	cmd.Execute()
}
