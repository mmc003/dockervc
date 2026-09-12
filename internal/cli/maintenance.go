package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"dockervc/internal/snapshot"
)

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Reclaim storage held by objects no snapshot references",
	Args:  cobra.NoArgs,
	Annotations: map[string]string{needsStore: "true"},
	RunE: func(cmd *cobra.Command, args []string) error {
		objs, err := st.UnreferencedObjects()
		if err != nil {
			return err
		}
		if len(objs) == 0 {
			fmt.Println("Nothing to prune — every object is referenced by a snapshot.")
			st.CleanEmptyShards()
			return nil
		}
		var total int64
		for _, o := range objs {
			total += o.Size
		}
		fmt.Printf("Found %d unreferenced object(s), %s.\n", len(objs), snapshot.HumanBytes(total))
		if !forceYes && !confirm("Delete them?") {
			fmt.Println("Aborted.")
			return nil
		}
		var fail int
		for _, o := range objs {
			if err := st.DeleteObject(o.Hash); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "failed to delete %s: %v\n", o.Hash[:12], err)
				fail++
			}
		}
		fmt.Printf("Reclaimed %s.\n", snapshot.HumanBytes(total))
		st.CleanEmptyShards()
		if fail > 0 {
			return fmt.Errorf("%d object(s) could not be deleted", fail)
		}
		return nil
	},
}

// verify is kept as a deprecated alias: the content re-hash it did is now
// `doctor --deep`, so there is one health-check command (restic/borg style).
var verifyCmd = &cobra.Command{
	Use:        "verify",
	Short:      "Re-hash every stored object (deprecated: use `doctor --deep`)",
	Deprecated: "`dockervc doctor --deep` replaces verify",
	Args:       cobra.NoArgs,
	Annotations: map[string]string{needsStore: "true"},
	RunE: func(cmd *cobra.Command, args []string) error {
		rep, err := diagnose(st, true)
		if err != nil {
			return err
		}
		printDoctorReport(cmd.OutOrStdout(), st, rep)
		if !rep.Healthy() {
			return fmt.Errorf("%d problem(s) found — run `dockervc doctor --repair` to fix them", rep.problemCount())
		}
		return nil
	},
}

var configCmd = &cobra.Command{
	Use:   "config [get <key> | set <key> <value> | unset <key>]",
	Short: "View or change store settings",
	Annotations: map[string]string{needsStore: "true"},
	Args:  cobra.RangeArgs(0, 3),
	RunE: func(cmd *cobra.Command, args []string) error {
		switch {
		case len(args) == 0:
			all, err := st.ConfigAll()
			if err != nil {
				return err
			}
			if len(all) == 0 {
				fmt.Println("(no settings)")
				return nil
			}
			for k, v := range all {
				fmt.Printf("%s = %s\n", k, v)
			}
			return nil
		case len(args) == 2 && args[0] == "get":
			v, ok, err := st.ConfigGet(args[1])
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("no setting named %q", args[1])
			}
			fmt.Println(v)
			return nil
		case len(args) == 3 && args[0] == "set":
			if err := validateConfig(args[1], args[2]); err != nil {
				return err
			}
			if err := st.ConfigSet(args[1], args[2]); err != nil {
				return err
			}
			fmt.Printf("%s = %s\n", args[1], args[2])
			return nil
		case len(args) == 2 && args[0] == "unset":
			if !knownSetting(args[1]) {
				return fmt.Errorf("unknown setting %q (known: zstd_level, export.folder)", args[1])
			}
			if err := st.ConfigDelete(args[1]); err != nil {
				return err
			}
			fmt.Printf("%s unset — back to its default\n", args[1])
			return nil
		default:
			return fmt.Errorf("usage: dockervc config [get <key> | set <key> <value> | unset <key>]")
		}
	},
}

// knownSetting reports whether key names a settable store setting (unset
// needs this check alone — there is no value to validate).
func knownSetting(key string) bool {
	switch key {
	case "zstd_level", "export.folder":
		return true
	}
	return false
}

func validateConfig(key, value string) error {
	switch key {
	case "zstd_level":
		var lvl int
		if _, err := fmt.Sscanf(value, "%d", &lvl); err != nil || lvl < 1 || lvl > 22 {
			return fmt.Errorf("zstd_level must be an integer 1–22")
		}
		return nil
	case "export.folder":
		if fi, err := os.Stat(expandHome(value)); err != nil || !fi.IsDir() {
			return fmt.Errorf("export.folder must be an existing directory (got %q)", value)
		}
		return nil
	default:
		return fmt.Errorf("unknown setting %q (known: zstd_level, export.folder)", key)
	}
}

func init() {
	pruneCmd.Flags().BoolVarP(&forceYes, "yes", "y", false, "skip confirmation")
	rootCmd.AddCommand(pruneCmd)
	rootCmd.AddCommand(verifyCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(diffCmd)
}
