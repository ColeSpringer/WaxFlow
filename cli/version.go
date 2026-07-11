package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

func newVersionCmd(version, flavorName string) *cobra.Command {
	name := "waxflow"
	if flavorName != "" {
		name += "-" + flavorName
	}
	return &cobra.Command{
		Use:   "version",
		Short: "Print version and build information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s %s/%s\n",
				name, version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}
