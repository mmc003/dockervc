// Package cli wires the dockervc command tree.
package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"dockervc/internal/store"
)

// st is the shared, lazily opened store handle for commands that need one.
var (
	storePath string
	st        *store.Store
	forceYes  bool
)

// needsStore marks commands that operate on an existing store.
const needsStore = "needs-store"

var rootCmd = &cobra.Command{
	Use:   "dockervc",
	Short: "Local version control and backup for Docker",
	Long: `dockervc snapshots, restores, exports and imports the state of a local
Docker engine — containers, images, volumes and networks — with git-like
snapshots stored entirely on this machine.`,
	SilenceUsage:  true,
	SilenceErrors: false,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Annotations[needsStore] != "true" {
			return nil
		}
		if st != nil { // defensive: never hold two opens
			st.Close()
			st = nil
		}
		s, err := store.Open(storePath)
		if err != nil {
			return err
		}
		st = s
		return nil
	},
	PersistentPostRun: func(cmd *cobra.Command, args []string) {
		if st != nil {
			st.Close()
			st = nil
		}
	},
}

// Execute runs the CLI.
func Execute() error {
	rootCmd.PersistentFlags().StringVar(&storePath, "store", store.DefaultPath(),
		"store path (overrides $DOCKERVC_HOME)")
	return rootCmd.Execute()
}

// confirm asks the user before a destructive action. It reads byte-wise and
// accepts either \n or \r as the terminator: in the TUI's raw mode Enter is
// \r (ICRNL off), and waiting for \n alone would hang forever.
func confirm(prompt string) bool {
	if tuiActive {
		// A console prompt inside the TUI is invisible (output is captured)
		// and its reader swallows every key but Enter, including Ctrl-C —
		// auto-decline instead; the TUI's dialogs collect the real answer.
		fmt.Printf("%s — prompt suppressed in TUI; rerun with --yes to force\n", prompt)
		return false
	}
	fmt.Printf("%s [y/N] ", prompt)
	var sb strings.Builder
	for {
		b, err := input().ReadByte()
		if err != nil || b == '\n' || b == '\r' {
			break
		}
		sb.WriteByte(b)
	}
	line := strings.TrimSpace(sb.String())
	return line == "y" || line == "Y" || line == "yes"
}

func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
