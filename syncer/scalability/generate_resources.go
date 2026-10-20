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
	"flag"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

const healthyStatus = `status:
  conditions:
  - lastTransitionTime: "1970-01-01T00:00:00Z"
    message: The resource is up to date
    reason: UpToDate
    status: "True"
    type: Ready`

type ResourceTemplate struct {
	ApiVersion string
	Spec       string
}

var templates = map[string]ResourceTemplate{
	"ArtifactRegistryRepository": {
		ApiVersion: "artifactregistry.cnrm.cloud.google.com/v1beta1",
		Spec:       "format: DOCKER\n  location: us-central1",
	},
	"PubSubTopic": {
		ApiVersion: "pubsub.cnrm.cloud.google.com/v1beta1",
		Spec:       "resourceID: {name}",
	},
	"PubSubSubscription": {
		ApiVersion: "pubsub.cnrm.cloud.google.com/v1beta1",
		Spec:       "topicRef:\n    name: sample-topic\n  ackDeadlineSeconds: 10",
	},
	"StorageBucket": {
		ApiVersion: "storage.cnrm.cloud.google.com/v1beta1",
		Spec:       "location: us-central1",
	},
	"BigQueryDataset": {
		ApiVersion: "bigquery.cnrm.cloud.google.com/v1beta1",
		Spec:       "location: us-central1",
	},
	"ComputeNetwork": {
		ApiVersion: "compute.cnrm.cloud.google.com/v1beta1",
		Spec:       "routingMode: REGIONAL",
	},
	"IAMServiceAccount": {
		ApiVersion: "iam.cnrm.cloud.google.com/v1beta1",
		Spec:       "displayName: Sample Service Account",
	},
	"RedisInstance": {
		ApiVersion: "redis.cnrm.cloud.google.com/v1beta1",
		Spec:       "region: us-central1\n  tier: BASIC\n  memorySizeGb: 1",
	},
	"KMSKeyRing": {
		ApiVersion: "kms.cnrm.cloud.google.com/v1beta1",
		Spec:       "location: us-central1",
	},
	"ComputeAddress": {
		ApiVersion: "compute.cnrm.cloud.google.com/v1beta1",
		Spec:       "location: us-central1",
	},
	"ComputeDisk": {
		ApiVersion: "compute.cnrm.cloud.google.com/v1beta1",
		Spec:       "location: us-central1\n  size: 10\n  type: pd-standard",
	},
	"ContainerCluster": {
		ApiVersion: "container.cnrm.cloud.google.com/v1beta1",
		Spec:       "location: us-central1\n  initialNodeCount: 1",
	},
	"EssentialContactsContact": {
		ApiVersion: "essentialcontacts.cnrm.cloud.google.com/v1beta1",
		Spec:       "email: test-email@test.com\n  languageTag: en\n  notificationCategorySubscriptions: [\"BILLING\"]\n  projectRef:\n   external: test-project",
	},
}

var fallbackApiVersions = map[string]string{
	"AlloyDBCluster":              "alloydb.cnrm.cloud.google.com/v1beta1",
	"AlloyDBInstance":             "alloydb.cnrm.cloud.google.com/v1beta1",
	"AlloyDBUser":                 "alloydb.cnrm.cloud.google.com/v1beta1",
	"CloudDeployDeliveryPipeline": "clouddeploy.cnrm.cloud.google.com/v1beta1",
	"APIGatewayAPI":               "apigateway.cnrm.cloud.google.com/v1beta1",
	"ApigeeOrganization":          "apigee.cnrm.cloud.google.com/v1beta1",
	"BigtableInstance":            "bigtable.cnrm.cloud.google.com/v1beta1",
	"SpannerInstance":             "spanner.cnrm.cloud.google.com/v1beta1",
	"SQLInstance":                 "sql.cnrm.cloud.google.com/v1beta1",
	"DNSManagedZone":              "dns.cnrm.cloud.google.com/v1beta1",
	"SecretManagerSecret":         "secretmanager.cnrm.cloud.google.com/v1beta1",
	"LoggingLogSink":              "logging.cnrm.cloud.google.com/v1beta1",
	"MonitoringAlertPolicy":       "monitoring.cnrm.cloud.google.com/v1beta1",
	"NetworkServicesMesh":         "networkservices.cnrm.cloud.google.com/v1beta1",
	"ServiceUsageService":         "serviceusage.cnrm.cloud.google.com/v1beta1",
	"VertexAIDataset":             "vertexai.cnrm.cloud.google.com/v1beta1",
	"WorkflowsWorkflow":           "workflows.cnrm.cloud.google.com/v1beta1",
	"ComputeFirewall":             "compute.cnrm.cloud.google.com/v1beta1",
	"ComputeRoute":                "compute.cnrm.cloud.google.com/v1beta1",
	"ComputeTargetHTTPProxy":      "compute.cnrm.cloud.google.com/v1beta1",
	"ComputeURLMap":               "compute.cnrm.cloud.google.com/v1beta1",
	"ComputeBackendService":       "compute.cnrm.cloud.google.com/v1beta1",
	"CloudFunctionsFunction":      "cloudfunctions.cnrm.cloud.google.com/v1beta1",
	"CloudIdentityGroup":          "cloudidentity.cnrm.cloud.google.com/v1beta1",
	"DatastreamStream":            "datastream.cnrm.cloud.google.com/v1alpha1",
	"DataflowFlexTemplateJob":     "dataflow.cnrm.cloud.google.com/v1beta1",
	"DataprocCluster":             "dataproc.cnrm.cloud.google.com/v1beta1",
	"FirestoreDatabase":           "firestore.cnrm.cloud.google.com/v1beta1",
	"GKEHubMembership":            "gkehub.cnrm.cloud.google.com/v1beta1",
	"IAPSettings":                 "iap.cnrm.cloud.google.com/v1beta1",
	"PrivateCACAPool":             "privateca.cnrm.cloud.google.com/v1beta1",
	"RunService":                  "run.cnrm.cloud.google.com/v1beta1",
}

func generateResource(kind, name string) string {
	var apiVersion, specBody string
	if t, ok := templates[kind]; ok {
		apiVersion = t.ApiVersion
		specBody = strings.ReplaceAll(t.Spec, "{name}", name)
	} else {
		apiVersion = fallbackApiVersions[kind]
		if apiVersion == "" {
			apiVersion = "cnrm.cloud.google.com/v1beta1"
		}
		specBody = "resourceID: " + name
	}

	now := time.Now().UTC().Format(time.RFC3339)
	status := strings.ReplaceAll(healthyStatus, "1970-01-01T00:00:00Z", now)

	return fmt.Sprintf("apiVersion: %s\nkind: %s\nmetadata:\n  name: %s\nspec:\n  %s\n%s\n",
		apiVersion, kind, name, specBody, status)
}

func main() {
	count := flag.Int("count", 1, "Total number of resources to generate")
	kindsStr := flag.String("kinds", "ArtifactRegistryRepository", "Comma separated list of resource kinds")
	numKinds := flag.Int("num-kinds", 0, "Pick N random kinds from the pool (overrides -kinds)")
	prefix := flag.String("prefix", "sample", "Prefix for resource names")

	flag.Parse()

	var selectedKinds []string

	// Build pool of all available kinds
	poolMap := make(map[string]bool)
	for k := range templates {
		poolMap[k] = true
	}
	for k := range fallbackApiVersions {
		poolMap[k] = true
	}
	var pool []string
	for k := range poolMap {
		pool = append(pool, k)
	}
	sort.Strings(pool)

	if *numKinds > 0 {
		if *numKinds > len(pool) {
			*numKinds = len(pool)
		}
		perm := rand.Perm(len(pool))
		for i := 0; i < *numKinds; i++ {
			selectedKinds = append(selectedKinds, pool[perm[i]])
		}
	} else {
		selectedKinds = strings.Split(*kindsStr, ",")
	}

	for i := 0; i < *count; i++ {
		kind := selectedKinds[rand.Intn(len(selectedKinds))]
		name := fmt.Sprintf("%s-%s-%d", *prefix, strings.ToLower(kind), i)
		fmt.Print(generateResource(kind, name))
		if i < *count-1 {
			fmt.Println("---")
		}
	}
}
