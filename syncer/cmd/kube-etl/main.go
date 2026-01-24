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

	rootCmd.AddCommand(export.BuildExportCommand())

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
