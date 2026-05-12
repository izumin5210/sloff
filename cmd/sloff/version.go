package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// buildVersion is the sloff binary version exposed as the OpenTelemetry
// `service.version` resource attribute. Defaults to "dev" for unreleased
// builds; release pipelines override it via `-ldflags "-X main.buildVersion=..."`.
var buildVersion = "dev"

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the sloff binary version",
		RunE: func(cobraCmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cobraCmd.OutOrStdout(), buildVersion)
			return err
		},
	}
}
