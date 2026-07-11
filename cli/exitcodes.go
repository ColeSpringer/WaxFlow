package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxflow/waxerr"
)

func newExitCodesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "exit-codes",
		Short: "Print the documented CLI exit-code contract",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "EXIT\tCLASS\tERROR CODES")
			for _, class := range waxerr.ExitContract() {
				codes := "-"
				if len(class.Codes) > 0 {
					parts := make([]string, len(class.Codes))
					for i, c := range class.Codes {
						parts[i] = string(c)
					}
					codes = strings.Join(parts, ", ")
				}
				if class.Exit == 1 {
					codes += ", any unclassified error"
				}
				fmt.Fprintf(w, "%d\t%s\t%s\n", class.Exit, class.Name, codes)
			}
			return w.Flush()
		},
	}
}
