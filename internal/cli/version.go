package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/arasHi87/gogolook-task-api/internal/buildinfo"
)

func newVersionCommand() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := buildinfo.Get()
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			}
			_, err := fmt.Fprintln(out, info)
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print build information as JSON")
	return cmd
}
