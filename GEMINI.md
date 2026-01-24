# kube-etl Architecture and Goals

`kube-etl` is a tool for Extract, Transform, Load (ETL) operations on Kubernetes objects.

## Goals

*   Provide a CLI tool `kube-etl` for easy interaction.
*   Support exporting Kubernetes objects from a cluster to various sinks.
*   Future support for transformations and loading into other clusters or storage.

## Architecture

The project is organized into the following components:

*   **Root**: The root directory contains the `kube-etl` CLI and its core logic.
    *   **main.go**: The main entry point for the `kube-etl` CLI.
    *   **pkg/export/**: Logic for exporting Kubernetes objects.
    *   **pkg/sink/**: Interfaces and implementations for data sinks (e.g., ZipSink).
*   **syncer/**: This directory contains the `KRMSyncer` controller (module `github.com/gke-labs/kube-etl/syncer`).
    *   **main.go**: The entry point for the controller manager.
    *   **api/**: API definitions for the controller.
    *   **controllers/**: Controller logic for copying kube objects between clusters.
