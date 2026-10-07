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
	if agent.IsRevoked(err) {
		newCredentialStore(*cfg, resolvedConfigPath()).wipe()
		cfg.Auth.APIKey, cfg.Agent.ID = "", ""
		appCfg.Auth.APIKey, appCfg.Agent.ID = "", ""
	} else if !errors.As(err, &httpError) || httpError.StatusCode != 401 {
		return err
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
	if err := mountAPIClient(*cfg).MountAccess(probe); err != nil {
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
	return ensureMountDependencies(ctx)
}

func ensureMountDependencies(ctx context.Context) (string, error) {
	return mountsetup.Ensure(ctx, mountsetup.Options{
		Directory: filepath.Join(filepath.Dir(resolvedConfigPath()), "tools"),
		Output:    os.Stdout, Confirm: confirmMountInstall,
	})
}

func runMountCommand(cmd *cobra.Command, args []string) error {
	if err := persistentMountConfig(); err != nil {
		return err
	}
	ctx, stop := mountContext(cmd.Context())
	defer stop()
	cfg := loadConfig()
	if err := mountDaemonPreflight(cfg); err != nil {
		return err
	}
	if err := enableRemoteMount(&cfg); err != nil {
		return err
	}
	if err := checkMountAccount(ctx, &cfg); err != nil {
		return err
	}
	if cfg.Agent.ID == "" {
		return errors.New("the background agent requires a registered identity; run unarr init before mounting")
	}
	if len(args) == 0 && cfg.Mount.Directory != "" {
		args = []string{cfg.Mount.Directory}
	}
	directory, err := mountDestination(args)
	if err != nil {
		return err
	}
	cfg.Mount.Directory = directory
	if err := savePreparedMount(ctx, cfg, ensureMountDependencies); err != nil {
		return err
	}
	if err := ensurePersistentMountService(); err != nil {
		return fmt.Errorf("mount was configured at %s, but the background service could not start: %w", directory, err)
	}
	fmt.Printf("Remote folder configured at %s; background activation requested.\n", directory)
	fmt.Println("Run unarr umount to disable it.")
	return nil
}

// Persist the enabled intention once the complete settings and dependency
// preflight succeed. Refusing setup or cancelling leaves the old mount intact.
func savePreparedMount(ctx context.Context, cfg config.Config, prepare func(context.Context) (string, error)) error {
	if err := cfg.Mount.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := prepare(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := config.Save(cfg, resolvedConfigPath()); err != nil {
		return fmt.Errorf("save mount activation: %w", err)
	}
	appCfg = cfg
	return nil
}

func ensurePersistentMountService() error {
	if mountServiceInstalled() {
		return runDaemonSvcRestart()
	}
	return runDaemonInstall()
}

func newUmountCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "umount",
		Aliases: []string{"unmount"},
		GroupID: "daemon",
		Short:   "Disable and unmount the persistent remote folder",
		Args:    cobra.NoArgs,
		RunE:    runUmountCommand,
	}
}

func runUmountCommand(_ *cobra.Command, _ []string) error {
	if err := persistentMountConfig(); err != nil {
		return err
	}
	cfg := loadConfig()
	if errCfgLoad != nil {
		return fmt.Errorf("read agent config: %w", errCfgLoad)
	}
	installed, active, err := umountServiceState()
	if err != nil {
		return err
	}
	if cfg.Mount.Enabled {
		cfg.Mount.Enabled = false
		if err := config.Save(cfg, resolvedConfigPath()); err != nil {
			return fmt.Errorf("disable remote folder: %w", err)
		}
		appCfg = cfg
	}
	return finishUmount(installed, active)
}

func umountServiceState() (installed, active bool, err error) {
	installed = mountServiceInstalled()
	if installed {
		active, err = mountServiceActive()
	}
	return installed, active, err
}

func finishUmount(installed, active bool) error {
	if installed && active {
		if err := runDaemonSvcRestart(); err != nil {
			return fmt.Errorf("remote folder was disabled, but the agent could not restart to unmount it: %w", err)
		}
		fmt.Println("Remote folder disabled; agent restart requested to unmount it.")
		return nil
	}
	if installed || !isDaemonAlive(agent.ReadState()) {
		fmt.Println("Remote folder disabled; the agent remains stopped.")
		return nil
	}
	return errors.New("remote folder disabled; restart the foreground unarr agent to finish unmounting it")
}

func enableRemoteMount(cfg *config.Config) error {
	if cfg.Mount.Enabled {
		return nil
	}
	if !isTerminal() {
		return errors.New("remote mount is disabled; run unarr config mount to enable it")
	}
	accepted := false
	if err := huh.NewConfirm().Title("Enable the optional remote folder?").Affirmative("Enable").Negative("Cancel").Value(&accepted).Run(); err != nil {
		return err
	}
	if !accepted {
		return errors.New("remote mount remains disabled")
	}
	cfg.Mount.Enabled = true
	return nil
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
