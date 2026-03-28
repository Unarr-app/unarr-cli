package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/fatih/color"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/torrentclaw-cli/internal/agent"
	"github.com/torrentclaw/torrentclaw-cli/internal/config"
)

func newSetupCmd() *cobra.Command {
	var apiURL string

	cmd := &cobra.Command{
		Use:   "setup",
		Short: "First-time configuration wizard",
		Long:  "Interactive setup that configures API key, download directory, and preferred download method.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSetup(apiURL)
		},
	}

	cmd.Flags().StringVar(&apiURL, "api-url", "", "API URL override (default: https://torrentclaw.com)")

	return cmd
}

func runSetup(apiURLOverride string) error {
	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)
	cyan := color.New(color.FgCyan)

	fmt.Println()
	bold.Println("  unarr Setup")
	fmt.Println()

	cfg := loadConfig()

	// Determine API URL
	apiURL := cfg.Auth.APIURL
	if apiURLOverride != "" {
		apiURL = apiURLOverride
	}
	if apiURL == "" {
		apiURL = "https://torrentclaw.com"
	}

	// Open browser to API keys page
	keysURL := apiURL + "/profile?tab=apikey"
	fmt.Printf("  Opening %s ...\n", keysURL)
	openBrowser(keysURL)
	fmt.Println()

	// Step 1: API Key
	apiKey := cfg.Auth.APIKey
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("API Key").
				Description("Copy it from the page that just opened in your browser").
				Placeholder("tc_...").
				Value(&apiKey).
				Validate(func(s string) error {
					s = strings.TrimSpace(s)
					if s == "" {
						return fmt.Errorf("API key is required")
					}
					if !strings.HasPrefix(s, "tc_") {
						return fmt.Errorf("API key should start with tc_")
					}
					return nil
				}),
		),
	).Run()
	if err != nil {
		return err
	}
	apiKey = strings.TrimSpace(apiKey)

	// Validate API key by registering with the server
	fmt.Print("  Verifying API key... ")

	agentID := cfg.Agent.ID
	if agentID == "" {
		agentID = uuid.New().String()
	}

	hostname, _ := os.Hostname()
	agentName := cfg.Agent.Name
	if agentName == "" {
		agentName = hostname
	}

	ac := agent.NewClient(apiURL, apiKey, "unarr/"+Version)
	resp, err := ac.Register(context.Background(), agent.RegisterRequest{
		AgentID:     agentID,
		Name:        agentName,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Version:     Version,
		DownloadDir: cfg.Download.Dir,
	})
	if err != nil {
		color.Red("FAILED")
		fmt.Println()
		return fmt.Errorf("API key validation failed: %w", err)
	}

	green.Println("OK")
	fmt.Printf("  Connected as %s (%s) [%s]\n", resp.User.Name, resp.User.Email, strings.ToUpper(resp.User.Plan))
	fmt.Println()

	// Step 2: Download directory
	downloadDir := cfg.Download.Dir
	if downloadDir == "" {
		downloadDir = defaultDownloadDir()
	}
	err = huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Download Directory").
				Description("Where should downloaded files be saved?").
				Value(&downloadDir),
		),
	).Run()
	if err != nil {
		return err
	}
	downloadDir = expandHome(strings.TrimSpace(downloadDir))

	// Step 3: Preferred download method
	method := cfg.Download.PreferredMethod
	if method == "" {
		method = "auto"
	}

	methodOptions := []huh.Option[string]{
		huh.NewOption("Auto (torrent, debrid when available)", "auto"),
		huh.NewOption("Torrent only (BitTorrent P2P)", "torrent"),
	}
	if resp.Features.Debrid {
		methodOptions = append(methodOptions,
			huh.NewOption("Debrid only (Real-Debrid, AllDebrid...)", "debrid"),
		)
	}
	if resp.Features.Usenet {
		methodOptions = append(methodOptions,
			huh.NewOption("Usenet only (requires Pro)", "usenet"),
		)
	}

	err = huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Download Method").
				Description("How do you want to download?").
				Options(methodOptions...).
				Value(&method),
		),
	).Run()
	if err != nil {
		return err
	}

	// Step 4: Agent name
	err = huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Device Name").
				Description("A name for this machine (shown in the web dashboard)").
				Value(&agentName),
		),
	).Run()
	if err != nil {
		return err
	}

	// Save config
	cfg.Auth.APIKey = apiKey
	cfg.Auth.APIURL = apiURL
	cfg.Agent.ID = agentID
	cfg.Agent.Name = strings.TrimSpace(agentName)
	cfg.Download.Dir = downloadDir
	cfg.Download.PreferredMethod = method

	// Set organize dirs based on download dir
	if cfg.Organize.MoviesDir == "" {
		cfg.Organize.MoviesDir = filepath.Join(downloadDir, "Movies")
	}
	if cfg.Organize.TVShowsDir == "" {
		cfg.Organize.TVShowsDir = filepath.Join(downloadDir, "TV Shows")
	}

	// Validate paths before saving
	if err := cfg.ValidatePaths(); err != nil {
		return fmt.Errorf("unsafe configuration: %w", err)
	}

	configPath := config.FilePath()
	if cfgFile != "" {
		configPath = cfgFile
	}

	if err := config.Save(cfg, configPath); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	// Summary
	fmt.Println()
	green.Println("  Setup complete!")
	fmt.Println()
	fmt.Printf("  User:      %s (%s) [%s]\n", resp.User.Name, resp.User.Email, strings.ToUpper(resp.User.Plan))
	fmt.Printf("  Downloads: %s\n", downloadDir)
	fmt.Printf("  Method:    %s\n", method)
	fmt.Printf("  Agent:     %s (%s)\n", agentName, agentID[:8]+"...")
	fmt.Printf("  Config:    %s\n", configPath)
	fmt.Println()

	// Features summary
	features := []string{}
	if resp.Features.Torrent {
		features = append(features, "Torrent")
	}
	if resp.Features.Debrid {
		features = append(features, "Debrid")
	}
	if resp.Features.Usenet {
		features = append(features, "Usenet")
	}
	cyan.Printf("  Available: %s\n", strings.Join(features, ", "))
	fmt.Println()
	fmt.Println("  Next: run", bold.Sprint("unarr daemon start"), "to begin downloading")
	fmt.Println()

	return nil
}

// openBrowser opens a URL in the default browser.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default: // linux, freebsd
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Start() // fire and forget
}

func defaultDownloadDir() string {
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, "Media"),
		filepath.Join(home, "Downloads", "unarr"),
	}
	for _, d := range candidates {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
	}
	return filepath.Join(home, "Media")
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}
