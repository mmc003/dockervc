package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Step 4 (export/import) of the build-out. Registered now so the command
// surface is discoverable, implemented next. Rollback (Step 3) moved to
// rollback.go.

var exportCmd = &cobra.Command{
	Use:   "export <snapshot>",
	Short: "Package a snapshot into a portable .dvca archive",
	Args:  cobra.MaximumNArgs(1),
	Annotations: map[string]string{needsStore: "true"},
	RunE: func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("export ships in Step 4 of the build-out (Portability)")
	},
}

var importCmd = &cobra.Command{
	Use:   "import <archive.dvca>",
	Short: "Add an exported archive's snapshots to this machine's store",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("import ships in Step 4 of the build-out (Portability)")
	},
}

func init() {
	rootCmd.AddCommand(exportCmd, importCmd)
}
