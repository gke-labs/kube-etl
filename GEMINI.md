# kube-etl Architecture and Goals

`kube-etl` is a tool for Extract, Transform, Load (ETL) operations on Kubernetes objects.

## Goals

*   Provide a CLI tool `kube-etl` for easy interaction.
*   Support exporting Kubernetes objects from a cluster to various sinks.
*   Future support for transformations and loading into other clusters or storage.

## Architecture

The project is organized into the following components:

*   **syncer/**: This directory contains the Go module for the project.
    *   **cmd/kube-etl/**: The main entry point for the `kube-etl` CLI.
    *   **pkg/export/**: Logic for exporting Kubernetes objects.
    *   **pkg/sink/**: Interfaces and implementations for data sinks (e.g., ZipSink).
    *   **controllers/**: Contains a controller for copying kube objects between clusters (existing subproject).
