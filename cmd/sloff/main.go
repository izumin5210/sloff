// Command sloff is the fingerprint-aware codegen orchestrator CLI.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	err := newRootCmd().Execute()
	if err == nil {
		return
	}
	if ec, ok := errors.AsType[*exitCodeError](err); ok {
		os.Exit(ec.code)
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// exitCodeError carries a custom process exit code without an accompanying
// stderr message. Subcommands that own their stdout output (e.g. `fingerprint diff`)
// return this so main can set the exit code without printing a generic error.
type exitCodeError struct{ code int }

func (e *exitCodeError) Error() string { return fmt.Sprintf("exit code %d", e.code) }

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "sloff",
		Short:         "Fingerprint-aware codegen orchestrator for monorepos",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(newRunCmd())
	cmd.AddCommand(newGraphCmd())
	cmd.AddCommand(newFingerprintCmd())
	return cmd
}
