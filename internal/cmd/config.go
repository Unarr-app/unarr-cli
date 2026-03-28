package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/torrentclaw-cli/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Configure unarr",
		Long: `Interactive setup for unarr.

Configures the API URL, API key, default country, and saves to config file.`,
		Example: `  unarr config`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfig()
		},
	}

	return cmd
}

func runConfig() error {
	if !isTerminal() {
		return fmt.Errorf("interactive config requires a terminal (use --api-key flag or env vars instead)")
	}

	reader := bufio.NewReader(os.Stdin)
	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)

	cfg := loadConfig()

	fmt.Println()
	bold.Println("  unarr Configuration")
	fmt.Println()

	// API URL
	currentURL := cfg.Auth.APIURL
	fmt.Printf("  API URL [%s]: ", currentURL)
	apiURL, _ := reader.ReadString('\n')
	apiURL = strings.TrimSpace(apiURL)
	if apiURL == "" {
		apiURL = currentURL
	}

	// API Key
	currentKey := cfg.Auth.APIKey
	keyDisplay := ""
	if currentKey != "" {
		if len(currentKey) > 8 {
			keyDisplay = currentKey[:8] + "..."
		} else {
			keyDisplay = currentKey
		}
	}
	fmt.Printf("  API Key [%s]: ", keyDisplay)
	apiKey, _ := reader.ReadString('\n')
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		apiKey = currentKey
	}

	// Country
	currentCountry := cfg.General.Country
	fmt.Printf("  Default country [%s]: ", currentCountry)
	country, _ := reader.ReadString('\n')
	country = strings.TrimSpace(country)
	if country == "" {
		country = currentCountry
	}

	// Apply changes
	cfg.Auth.APIURL = apiURL
	cfg.Auth.APIKey = apiKey
	cfg.General.Country = country

	// Save
	configPath := config.FilePath()
	if cfgFile != "" {
		configPath = cfgFile
	}

	if err := config.Save(cfg, configPath); err != nil {
		return fmt.Errorf("could not save config: %w", err)
	}

	fmt.Println()
	green.Printf("  Configuration saved to %s\n", configPath)
	fmt.Println()

	return nil
}

// isTerminal checks if stdin is a terminal.
func isTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
