package cmd

import "github.com/spf13/cobra"

// completionCountryCodes provides shell completion for --country flags.
func completionCountryCodes(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return []string{
		"US\tUnited States", "GB\tUnited Kingdom", "ES\tSpain", "FR\tFrance",
		"DE\tGermany", "IT\tItaly", "PT\tPortugal", "BR\tBrazil",
		"MX\tMexico", "AR\tArgentina", "CA\tCanada", "AU\tAustralia",
		"NL\tNetherlands", "SE\tSweden", "NO\tNorway", "DK\tDenmark",
		"FI\tFinland", "JP\tJapan", "KR\tSouth Korea", "IN\tIndia",
	}, cobra.ShellCompDirectiveNoFileComp
}
