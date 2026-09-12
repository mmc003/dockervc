package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Version is set at build time via:
//
//	-ldflags "-X dockervc/internal/cli.Version=1.2.3"
var Version = "dev"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print dockervc version information",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("dockervc %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
	},
}

func init() { rootCmd.AddCommand(versionCmd) }
