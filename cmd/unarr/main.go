package main

import (
	"os"

	"github.com/Unarr-app/unarr-cli/internal/cmd"
	"github.com/Unarr-app/unarr-cli/internal/engine"
	"github.com/Unarr-app/unarr-cli/internal/sentry"
)

func main() {
	// Checker child: the daemon re-execs itself to integrity-check the bolt
	// piece-completion DB out of process (a torn page can fault the checker,
	// which must not take the daemon down). No sentry, no cobra — the parent
	// reads the exit code and stdout only.
	if path := os.Getenv(engine.BoltCheckChildEnv); path != "" {
		os.Exit(engine.BoltCheckMain(path))
	}

	sentry.Init(cmd.Version)
	defer sentry.Close()
	defer sentry.RecoverPanic()

	cmd.Execute()
}
