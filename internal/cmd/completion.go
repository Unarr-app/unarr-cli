package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newCompletionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion <bash|zsh|fish|powershell>",
		Short: "Generate shell completion scripts",
		Long: `Generate shell completion scripts for unarr.

Completions allow you to press Tab to auto-complete commands, flags,
and arguments in your terminal. Follow the instructions for your shell below.

Bash:
  # Add to ~/.bashrc for persistent completions:
  echo 'eval "$(unarr completion bash)"' >> ~/.bashrc

  # Or generate a file (recommended for system-wide):
  unarr completion bash > /etc/bash_completion.d/unarr

Zsh:
  # Add to ~/.zshrc for persistent completions:
  echo 'eval "$(unarr completion zsh)"' >> ~/.zshrc

  # Or if you use oh-my-zsh, place in custom completions dir:
  mkdir -p ${ZSH_CUSTOM:-~/.oh-my-zsh/custom}/completions
  unarr completion zsh > ${ZSH_CUSTOM:-~/.oh-my-zsh/custom}/completions/_unarr

Fish:
  # Add to fish completions dir:
  unarr completion fish > ~/.config/fish/completions/unarr.fish

PowerShell:
  # Add to your PowerShell profile:
  unarr completion powershell >> $PROFILE`,
		Example: `  unarr completion bash
  unarr completion zsh
  unarr completion fish > ~/.config/fish/completions/unarr.fish
  eval "$(unarr completion bash)"`,
		Args:                  cobra.ExactArgs(1),
		DisableFlagsInUseLine: true,
		ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return rootCmd.GenBashCompletionV2(os.Stdout, true)
			case "zsh":
				return rootCmd.GenZshCompletion(os.Stdout)
			case "fish":
				return rootCmd.GenFishCompletion(os.Stdout, true)
			case "powershell":
				return rootCmd.GenPowerShellCompletionWithDesc(os.Stdout)
			default:
				return fmt.Errorf("unknown shell %q: must be one of bash, zsh, fish, powershell", args[0])
			}
		},
	}

	return cmd
}
