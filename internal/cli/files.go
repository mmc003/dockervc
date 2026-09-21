package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"dockervc/internal/artifact"
	"dockervc/internal/model"
	"dockervc/internal/snapshot"
)

var filesCmd = &cobra.Command{
	Use:         "files <snapshot|latest> <volume> [prefix]",
	Short:       "List files captured in a snapshot volume",
	Args:        cobra.RangeArgs(2, 3),
	Annotations: map[string]string{needsStore: "true"},
	RunE: func(cmd *cobra.Command, args []string) error {
		var m *model.Manifest
		var err error
		if strings.EqualFold(args[0], "latest") {
			m, err = st.LatestSnapshot()
			if err == nil && m == nil {
				return fmt.Errorf("no snapshots yet — take one first")
			}
		} else {
			m, err = st.GetSnapshot(args[0])
		}
		if err != nil {
			return err
		}
		_, volume, err := artifact.SelectVolume(m, args[1])
		if err != nil {
			return err
		}
		if volume.IndexObject == "" {
			return fmt.Errorf("snapshot %s has no file index for volume %s", m.ID, volume.Name)
		}
		prefix := ""
		if len(args) == 3 {
			prefix, err = artifact.NormalizePath(args[2])
			if err != nil {
				return err
			}
		}
		idx, err := snapshot.LoadFileIndex(st.OpenObject, volume)
		if err != nil {
			return fmt.Errorf("load file index for %s: %w", volume.Name, err)
		}
		return printArchiveFiles(m.ID, volume.Name, prefix, idx)
	},
}

func init() { rootCmd.AddCommand(filesCmd) }
