// Command sloff is the cache-aware codegen orchestrator CLI.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "sloff",
		Short:         "Cache-aware codegen orchestrator for monorepos",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(newRunCmd())
	cmd.AddCommand(newGraphCmd())
	return cmd
}
