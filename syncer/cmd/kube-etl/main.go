package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/gke-labs/kube-etl/pkg/export"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "kube-etl",
		Short: "A tool for ETL operations on Kubernetes objects",
	}

	var output string

	exportCmd := &cobra.Command{
		Use:   "export",
		Short: "Export Kubernetes objects to a sink",
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				return fmt.Errorf("required flag(s) \"output\" not set")
			}
			return export.Run(output)
		},
	}

	exportCmd.Flags().StringVar(&output, "output", "", "Path to the output file (e.g. output.zip)")

	rootCmd.AddCommand(exportCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
