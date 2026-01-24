package main

import (
	"fmt"
	"os"

	"github.com/gke-labs/kube-etl/pkg/export"
	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "kube-etl",
		Short: "A tool for ETL operations on Kubernetes objects",
	}

	rootCmd.AddCommand(BuildExportCommand())

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func BuildExportCommand() *cobra.Command {
	var opt export.ExportOptions

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export Kubernetes objects from the current cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			if opt.Output == "" {
				return fmt.Errorf("required flag(s) \"output\" not set")
			}
			return opt.Run(cmd.Context())
		},
	}

	cmd.Flags().StringVar(&opt.Output, "output", "", "Path to the output file (e.g. output.zip)")

	return cmd
}