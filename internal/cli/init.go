package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"dockervc/internal/store"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Create a new dockervc store",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := store.Init(storePath)
		if err != nil {
			return err
		}
		defer s.Close()
		fmt.Printf("Initialized empty dockervc store at %s\n", s.Path)
		fmt.Println("Take your first snapshot with: dockervc snapshot -m \"baseline\"")
		return nil
	},
}

func init() { rootCmd.AddCommand(initCmd) }
