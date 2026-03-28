package cmd

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

func newVersionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show unarr version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("unarr %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
		},
	}

	return cmd
}
