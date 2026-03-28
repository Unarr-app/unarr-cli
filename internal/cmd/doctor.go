package cmd

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/torrentclaw-cli/internal/agent"
	"github.com/torrentclaw/torrentclaw-cli/internal/config"
)

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose CLI configuration and connectivity",
		Long:  "Run diagnostic checks on API connectivity, config validity, disk space, and capabilities.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor()
		},
	}
}

func runDoctor() error {
	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)
	red := color.New(color.FgRed)
	yellow := color.New(color.FgYellow)

	fmt.Println()
	bold.Println("  unarr Diagnostics")
	fmt.Println()

	pass := 0
	fail := 0
	warn := 0

	check := func(name string, fn func() (string, error)) {
		msg, err := fn()
		if err != nil {
			red.Printf("  x %s", name)
			if msg != "" {
				fmt.Printf(" — %s", msg)
			}
			fmt.Println()
			fail++
		} else if msg != "" && msg[0] == '!' {
			yellow.Printf("  ! %s", name)
			fmt.Printf(" — %s", msg[1:])
			fmt.Println()
			warn++
		} else {
			green.Printf("  + %s", name)
			if msg != "" {
				fmt.Printf(" — %s", msg)
			}
			fmt.Println()
			pass++
		}
	}

	// Config
	bold.Println("  Config")
	cfg := loadConfig()

	check("Config file", func() (string, error) {
		path := config.FilePath()
		if cfgFile != "" {
			path = cfgFile
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path + " (not found, run unarr setup)", fmt.Errorf("missing")
		}
		return path, nil
	})

	check("API key configured", func() (string, error) {
		key := apiKeyFlag
		if key == "" {
			key = cfg.Auth.APIKey
		}
		if key == "" {
			return "run unarr setup to configure", fmt.Errorf("missing")
		}
		if len(key) > 8 {
			return key[:8] + "...", nil
		}
		return "set", nil
	})

	fmt.Println()
	bold.Println("  Connectivity")

	// API connectivity
	check("API reachable", func() (string, error) {
		client := getClient()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		start := time.Now()
		_, err := client.Health(ctx)
		elapsed := time.Since(start)
		if err != nil {
			return cfg.Auth.APIURL, err
		}
		return fmt.Sprintf("%s (%dms)", cfg.Auth.APIURL, elapsed.Milliseconds()), nil
	})

	// Agent registration
	check("Agent registration", func() (string, error) {
		key := apiKeyFlag
		if key == "" {
			key = cfg.Auth.APIKey
		}
		if key == "" {
			return "no API key", fmt.Errorf("skipped")
		}
		if cfg.Agent.ID == "" {
			return "no agent ID, run unarr setup", fmt.Errorf("not registered")
		}

		ac := agent.NewClient(cfg.Auth.APIURL, key, "unarr/"+Version)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp, err := ac.Register(ctx, agent.RegisterRequest{
			AgentID: cfg.Agent.ID,
			Name:    cfg.Agent.Name,
			OS:      runtime.GOOS,
			Arch:    runtime.GOARCH,
			Version: Version,
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s (%s) [%s]", resp.User.Name, resp.User.Email, resp.User.Plan), nil
	})

	fmt.Println()
	bold.Println("  Downloads")

	check("Download directory", func() (string, error) {
		dir := cfg.Download.Dir
		if dir == "" {
			return "not configured, run unarr setup", fmt.Errorf("missing")
		}
		fi, err := os.Stat(dir)
		if os.IsNotExist(err) {
			return dir + " (does not exist)", fmt.Errorf("missing")
		}
		if !fi.IsDir() {
			return dir + " (not a directory)", fmt.Errorf("invalid")
		}
		return dir, nil
	})

	check("Download dir writable", func() (string, error) {
		dir := cfg.Download.Dir
		if dir == "" {
			return "", fmt.Errorf("not configured")
		}
		tmpFile := dir + "/.unarr_write_test"
		f, err := os.Create(tmpFile)
		if err != nil {
			return "", fmt.Errorf("not writable: %w", err)
		}
		f.Close()
		os.Remove(tmpFile)
		return "OK", nil
	})

	check("Disk space", func() (string, error) {
		dir := cfg.Download.Dir
		if dir == "" {
			return "", fmt.Errorf("not configured")
		}
		var stat syscall.Statfs_t
		if err := syscall.Statfs(dir, &stat); err != nil {
			return "", err
		}
		available := int64(stat.Bavail) * int64(stat.Bsize)
		gb := float64(available) / (1024 * 1024 * 1024)
		msg := fmt.Sprintf("%.1f GB free", gb)
		if gb < 10 {
			return "!" + msg + " (low)", nil
		}
		return msg, nil
	})

	fmt.Println()
	bold.Println("  Version")

	check("unarr version", func() (string, error) {
		return fmt.Sprintf("%s (%s/%s)", Version, runtime.GOOS, runtime.GOARCH), nil
	})

	// Summary
	fmt.Println()
	if fail == 0 && warn == 0 {
		green.Println("  All checks passed!")
	} else if fail == 0 {
		yellow.Printf("  %d passed, %d warnings\n", pass, warn)
	} else {
		red.Printf("  %d passed, %d failed, %d warnings\n", pass, fail, warn)
	}
	fmt.Println()

	return nil
}
