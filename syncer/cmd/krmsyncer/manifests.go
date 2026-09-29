// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// generateSyncConfig returns the sample KRMSyncer CR that syncs PubSubTopics from the source cluster.
func generateSyncConfig(namespace string) string {
	return fmt.Sprintf(`apiVersion: syncer.gkelabs.io/v1alpha1
kind: KRMSyncer
metadata:
  name: %s
  namespace: %s
spec:
  mode: pull
  suspend: false
  remote:
    clusterConfig:
      kubeConfigSecretRef:
        name: %s
  rules:
  - group: pubsub.cnrm.cloud.google.com
    kind: PubSubTopic
    syncFields:
    - spec
    - status
    version: v1beta1
`, syncConfigName, namespace, sourceSecretName)
}

// generateSourceKubeconfig returns a kubeconfig for the source cluster that
// authenticates with Application Default Credentials. Inside the controller pod,
// ADC is provided by Workload Identity, so no user credentials are embedded.
func generateSourceKubeconfig(endpoint, caData string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: source
  cluster:
    server: https://%s
    certificate-authority-data: %s
contexts:
- name: source
  context:
    cluster: source
    user: source
current-context: source
users:
- name: source
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: gke-gcloud-auth-plugin
      args:
      - --use_application_default_credentials
      provideClusterInfo: true
      interactiveMode: Never
`, endpoint, caData)
}

// namespaceManifest returns a Namespace manifest.
func namespaceManifest(name string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
`, name)
}

// secretManifest returns an Opaque Secret manifest with a single data key.
func secretManifest(name, namespace, key string, value []byte) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
data:
  %s: %s
`, name, namespace, key, base64.StdEncoding.EncodeToString(value))
}

// substituteImage replaces the controller image placeholder in rendered manifests.
func substituteImage(manifests, placeholder, image string) (string, error) {
	needle := "image: " + placeholder
	if !strings.Contains(manifests, needle) {
		return "", fmt.Errorf("could not find image placeholder %q in rendered manifests", placeholder)
	}
	return strings.ReplaceAll(manifests, needle, "image: "+image), nil
}

// gkeContext returns the kubectl context name that `gcloud container clusters get-credentials` creates.
func gkeContext(project, location, cluster string) string {
	return fmt.Sprintf("gke_%s_%s_%s", project, location, cluster)
}

var gkeContextRegexp = regexp.MustCompile(`^gke_([^_]+)_([^_]+)_(.+)$`)

// parseGKEContext derives project/location/cluster from a GKE context name: gke_<project>_<location>_<cluster>.
func parseGKEContext(context string) (project, location, cluster string, ok bool) {
	m := gkeContextRegexp.FindStringSubmatch(context)
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}

// filterDigests returns the digests in the newline-separated list, excluding keep.
func filterDigests(list, keep string) []string {
	var out []string
	for _, d := range strings.Fields(list) {
		if d != keep {
			out = append(out, d)
		}
	}
	return out
}

// readGoMod returns the module path and go version declared in a go.mod file.
func readGoMod(path string) (module, goVersion string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	s := bufio.NewScanner(bytes.NewReader(data))
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "module":
			module = fields[1]
		case "go":
			goVersion = fields[1]
		}
	}
	return module, goVersion, s.Err()
}

// checkModuleDir verifies that dir is the krmsyncer module directory. The CLI
// must be run from there because it builds the image from the Dockerfile and
// renders manifests from config/.
func checkModuleDir(dir string) error {
	module, _, err := readGoMod(filepath.Join(dir, "go.mod"))
	if err != nil || module != krmsyncerModule {
		return fmt.Errorf("%s is not the krmsyncer directory (module %s); please run this command from the krmsyncer directory", dir, krmsyncerModule)
	}
	return nil
}
