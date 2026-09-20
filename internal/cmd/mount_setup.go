package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/mountsetup"
	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
)

func confirmMountInstall(explanation string) error {
	fmt.Println("\n" + explanation + "\n")
	if !isTerminal() {
		return errors.New("installation needs your approval; run unarr config mount in a terminal")
	}
	accepted := false
	err := huh.NewConfirm().Title("Continue with the required system setup?").Affirmative("Continue").Negative("Cancel").Value(&accepted).Run()
	if err != nil {
		return err
	}
	if !accepted {
		return errors.New("installation cancelled; no system changes made")
	}
	return nil
}

func checkMountAccount(ctx context.Context, cfg *config.Config) error {
	if cfg.Auth.APIKey == "" {
		if err := mountSignIn(cfg); err != nil {
			return err
		}
	}
	err := probeMountAccount(ctx, cfg)
	var httpError *agent.HTTPError
	if !errors.As(err, &httpError) || httpError.StatusCode != 401 {
		return err
	}
	if agent.IsRevoked(err) {
		clearRevokedIdentity(*cfg, "mount")
	}
	if err := mountSignIn(cfg); err != nil {
		return err
	}
	return probeMountAccount(ctx, cfg)
}

func mountSignIn(cfg *config.Config) error {
	if !isTerminal() {
		return errors.New("sign-in is required; run unarr config mount in a terminal")
	}
	fmt.Println("Sign-in is needed to verify your paid plan. Opening the existing browser sign-in flow...")
	if err := runLogin(cfg.Auth.APIURL, true); err != nil {
		return err
	}
	fresh := loadConfig()
	cfg.Auth, cfg.Agent = fresh.Auth, fresh.Agent
	if cfg.Auth.APIKey == "" {
		return errors.New("sign-in was not completed")
	}
	return nil
}

func probeMountAccount(ctx context.Context, cfg *config.Config) error {
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := agent.NewClient(cfg.Auth.APIURL, cfg.Auth.APIKey, "unarr-mount").MountAccess(probe); err != nil {
		return fmt.Errorf("mount access: %w", err)
	}
	return nil
}

func prepareMountDependencies(ctx context.Context, cfg *config.Config) (string, error) {
	// Free users and invalid sessions must never trigger an installation.
	if err := cfg.Mount.Validate(); err != nil {
		return "", err
	}
	if err := checkMountAccount(ctx, cfg); err != nil {
		return "", err
	}
	return mountsetup.Ensure(ctx, mountsetup.Options{
		Directory: filepath.Join(filepath.Dir(resolvedConfigPath()), "tools"),
		Output:    os.Stdout, Confirm: confirmMountInstall,
	})
}

func runMountCommand(cmd *cobra.Command, args []string) error {
	ctx, stop := mountContext(cmd.Context())
	defer stop()
	cfg := loadConfig()
	if err := enableRemoteMount(&cfg); err != nil {
		return err
	}
	binary, err := prepareMountDependencies(ctx, &cfg)
	if err != nil {
		return err
	}
	directory, err := mountDestination(args)
	if err != nil {
		return err
	}
	fmt.Printf("Mounting at %s. Keep this command running; Ctrl-C stops the mount.\n", directory)
	return runRemoteMount(ctx, cfg, directory, binary)
}

func enableRemoteMount(cfg *config.Config) error {
	if cfg.Mount.Enabled {
		return nil
	}
	if !isTerminal() {
		return errors.New("remote mount is disabled; run unarr config mount to enable it")
	}
	if err := configMount(cfg); err != nil {
		return err
	}
	if !cfg.Mount.Enabled {
		return errors.New("remote mount remains disabled")
	}
	return config.Save(*cfg, resolvedConfigPath())
}

func mountDestination(args []string) (string, error) {
	if len(args) > 0 {
		return createMountDirectory(expandHome(args[0]))
	}
	if runtime.GOOS == "windows" {
		for _, letter := range []string{"X:", "Y:", "Z:", "W:", "V:", "U:"} {
			if _, err := os.Stat(letter + `\`); errors.Is(err, os.ErrNotExist) {
				return letter, nil
			}
		}
		return "", errors.New("no default drive letter is free; run unarr mount with an unused drive letter")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return createMountDirectory(filepath.Join(home, "unarr-media"))
}

func createMountDirectory(dir string) (string, error) {
	if runtime.GOOS != "windows" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", err
		}
	}
	return validateMountPoint(dir)
}
