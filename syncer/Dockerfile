# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Build the manager binary
FROM golang:1.25 AS builder

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY main.go main.go
COPY api/ api/
COPY controllers/ controllers/

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager main.go

# Use Google Cloud SDK slim image as the base image to include gcloud
# and install the gke-gcloud-auth-plugin for authentication
FROM gcr.io/google.com/cloudsdktool/google-cloud-cli:slim

WORKDIR /

# Install the gke-gcloud-auth-plugin
RUN apt-get update && apt-get install -y google-cloud-cli-gke-gcloud-auth-plugin && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

# Add a non-root user to run the controller securely and ensure gcloud has a writable config dir
RUN useradd -u 65532 -m nonroot && \
    mkdir -p /home/nonroot/.config/gcloud && \
    chown -R 65532:65532 /home/nonroot/.config

COPY --from=builder /workspace/manager .

# Run as nonroot
USER 65532:65532

ENTRYPOINT ["/manager"]