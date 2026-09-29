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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// Controller deployment constants (must match config/).
const (
	krmsyncerModule      = "github.com/gke-labs/kube-etl/syncer"
	controllerNamespace  = "krmsyncer-system"
	controllerKSA        = "krmsyncer-controller-manager"
	controllerDeployment = "krmsyncer-controller-manager"
	imagePlaceholder     = "controller:latest"
	gsaName              = "krmsyncer"
	sourceSecretName     = "source-cluster"
	krmsyncerCRD         = "krmsyncers.syncer.gkelabs.io"
	syncConfigName       = "kcc-resource-syncer"
)

type syncOptions struct {
	// Flags
	SourceCluster  string
	SourceLocation string
	DestCluster    string
	DestLocation   string
	Project        string
	Namespace      string

	// Derived in complete()
	moduleDir     string
	sourceContext string
	destContext   string
	destProject   string
	imageRepo     string
	image         string
	gsaEmail      string
	wiMember      string
}

func buildSyncCommand() *cobra.Command {
	o := &syncOptions{}
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Deploy the KRMSyncer controller and a sample KRMSyncer CR into the destination cluster",
		Long: `Deploy the KRMSyncer controller and a sample KRMSyncer CR into the destination cluster.

Steps:
  1. Configure Workload Identity (GSA, IAM bindings) for the controller.
  2. Build and push the controller image, then delete older images.
  3. Create the source-cluster kubeconfig Secret on the destination cluster.
  4. Deploy the CRD, RBAC and controller into the destination cluster.
  5. Apply a sample KRMSyncer CR.`,
		Example: `  krmsyncer sync --source-cluster src --source-location us-west1 --project my-project \
    --dest-cluster dst --dest-location us-central1`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.run(cmd.Context())
		},
	}

	f := cmd.Flags()
	f.StringVarP(&o.SourceCluster, "source-cluster", "s", "", "Name of the source cluster")
	f.StringVar(&o.SourceLocation, "source-location", "", "GCP location for the source cluster (e.g. us-west1)")
	f.StringVarP(&o.DestCluster, "dest-cluster", "d", "", "Name of the destination cluster (defaults to the current kubectl context)")
	f.StringVar(&o.DestLocation, "dest-location", "", "GCP location for the destination cluster (e.g. us-central1)")
	f.StringVarP(&o.Project, "project", "j", "", "Google Cloud Project ID")
	f.StringVarP(&o.Namespace, "namespace", "n", controllerNamespace, "Namespace for the KRMSyncer CR and source-cluster Secret")
	for _, name := range []string{"source-cluster", "source-location", "project"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

func (o *syncOptions) run(ctx context.Context) error {
	if err := checkDeps("kubectl", "gcloud", "docker"); err != nil {
		return err
	}
	if err := o.complete(ctx); err != nil {
		return err
	}
	o.printSummary()

	steps := []struct {
		title string
		fn    func(context.Context) error
	}{
		// Workload Identity runs before the image build: new IAM bindings can take a
		// few minutes to propagate, and the build gives them time so the controller
		// doesn't hit 403 "iam.serviceAccounts.getAccessToken denied" errors on startup.
		{"Step 1: Configure Workload Identity", o.configureWorkloadIdentity},
		{"Step 2: Controller Image", o.buildAndPushImage},
		{"Step 3: Configure Remote Access Secret", o.configureSourceSecret},
		{"Step 4: Deploy KRMSyncer Controller", o.deployController},
		{"Step 5: Deploy KRMSyncer Configuration", o.deploySyncConfig},
	}
	for _, step := range steps {
		logHeader(step.title)
		if err := step.fn(ctx); err != nil {
			return err
		}
	}

	fmt.Println()
	logInfo("View controller logs with:")
	logInfo("  kubectl --context=%s -n %s logs deploy/%s -f", o.destContext, controllerNamespace, controllerDeployment)
	return nil
}

// complete validates the working directory and resolves kubectl contexts and derived names.
func (o *syncOptions) complete(ctx context.Context) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := checkModuleDir(wd); err != nil {
		return err
	}
	o.moduleDir = wd

	o.imageRepo = fmt.Sprintf("gcr.io/%s/krmsyncer/controller", o.Project)
	o.image = o.imageRepo + ":latest"

	o.sourceContext = gkeContext(o.Project, o.SourceLocation, o.SourceCluster)
	o.destProject = o.Project
	if o.DestCluster != "" && o.DestLocation != "" {
		o.destContext = gkeContext(o.Project, o.DestLocation, o.DestCluster)
	} else {
		// If dest cluster context is empty, fall back to current context.
		current, err := output(ctx, "kubectl", "config", "current-context")
		if err != nil {
			return fmt.Errorf("getting current kubectl context: %w", err)
		}
		o.destContext = current
		logInfo("No dest-cluster or dest-location is provided. Defaulting to current context: %s", current)
		if project, location, cluster, ok := parseGKEContext(current); ok {
			o.destProject, o.DestLocation, o.DestCluster = project, location, cluster
		} else {
			o.DestCluster = ""
			logWarn("Current context is not a GKE context; skipping the Workload Identity check on the destination cluster.")
		}
	}

	o.gsaEmail = fmt.Sprintf("%s@%s.iam.gserviceaccount.com", gsaName, o.Project)
	o.wiMember = fmt.Sprintf("serviceAccount:%s.svc.id.goog[%s/%s]", o.destProject, controllerNamespace, controllerKSA)
	return nil
}

func (o *syncOptions) printSummary() {
	fmt.Println("==================================================")
	fmt.Println("          KRMSyncer Deployment Setup              ")
	fmt.Println("==================================================")
	logInfo("Source Cluster Context   : %s", o.sourceContext)
	logInfo("Dest Cluster Context     : %s", o.destContext)
	logInfo("GCP Project ID           : %s", o.Project)
	logInfo("Syncer CR Namespace      : %s", o.Namespace)
	logInfo("Controller Namespace     : %s", controllerNamespace)
	logInfo("Controller Image         : %s", o.image)
	logInfo("Google Service Account   : %s", o.gsaEmail)
	logInfo("Module Directory         : %s", o.moduleDir)
	fmt.Println("==================================================")
}

func (o *syncOptions) configureWorkloadIdentity(ctx context.Context) error {
	if o.DestCluster != "" && o.DestLocation != "" {
		logInfo("Checking Workload Identity on destination cluster '%s'...", o.DestCluster)
		pool, err := output(ctx, "gcloud", "container", "clusters", "describe", o.DestCluster,
			"--location", o.DestLocation, "--project", o.destProject,
			"--format=value(workloadIdentityConfig.workloadPool)")
		if err != nil {
			return err
		}
		if pool == "" {
			return fmt.Errorf("workload Identity is not enabled on destination cluster %q; enable it with:\n"+
				"  gcloud container clusters update %s --location %s --project %s --workload-pool=%s.svc.id.goog\n"+
				"(existing node pools also need --workload-metadata=GKE_METADATA)",
				o.DestCluster, o.DestCluster, o.DestLocation, o.destProject, o.destProject)
		}
		logSuccess("Workload Identity enabled (pool: %s).", pool)
	}

	if err := quiet(ctx, "gcloud", "iam", "service-accounts", "describe", o.gsaEmail, "--project", o.Project); err == nil {
		logInfo("Google Service Account %s already exists.", o.gsaEmail)
	} else {
		logInfo("Creating Google Service Account %s...", o.gsaEmail)
		if err := run(ctx, "gcloud", "iam", "service-accounts", "create", gsaName, "--project", o.Project,
			"--display-name", "KRMSyncer controller"); err != nil {
			return err
		}
	}

	logInfo("Granting roles/container.viewer on project %s to %s (read access to the source cluster)...", o.Project, o.gsaEmail)
	if err := quiet(ctx, "gcloud", "projects", "add-iam-policy-binding", o.Project,
		"--member", "serviceAccount:"+o.gsaEmail,
		"--role", "roles/container.viewer",
		"--condition=None", "--quiet"); err != nil {
		return err
	}

	logInfo("Allowing %s to impersonate %s...", o.wiMember, o.gsaEmail)
	if err := quiet(ctx, "gcloud", "iam", "service-accounts", "add-iam-policy-binding", o.gsaEmail, "--project", o.Project,
		"--member", o.wiMember,
		"--role", "roles/iam.workloadIdentityUser",
		"--condition=None", "--quiet"); err != nil {
		return err
	}
	logSuccess("Workload Identity configured.")
	return nil
}

func (o *syncOptions) buildAndPushImage(ctx context.Context) error {
	// Remove the stale local image so rebuilding the same tag doesn't leave dangling images.
	if err := quiet(ctx, "docker", "image", "inspect", o.image); err == nil {
		logInfo("Removing local image %s...", o.image)
		if err := quiet(ctx, "docker", "image", "rm", o.image); err != nil {
			return err
		}
	}

	_, goVersion, err := readGoMod(filepath.Join(o.moduleDir, "go.mod"))
	if err != nil {
		return fmt.Errorf("reading go.mod: %w", err)
	}
	if goVersion == "" {
		return fmt.Errorf("no go directive found in %s", filepath.Join(o.moduleDir, "go.mod"))
	}
	logInfo("Building controller image %s (Go %s)...", o.image, goVersion)
	if err := run(ctx, "docker", "build", "--platform", "linux/amd64",
		"--build-arg", "GO_VERSION="+goVersion, "-t", o.image, o.moduleDir); err != nil {
		return err
	}
	logInfo("Pushing controller image %s...", o.image)
	if err := run(ctx, "docker", "push", o.image); err != nil {
		return fmt.Errorf("failed to push %s (you may need to run: gcloud auth configure-docker gcr.io): %w", o.image, err)
	}
	logSuccess("Controller image pushed.")

	// Re-pushing :latest leaves the previous digest untagged in the registry.
	// Prune every digest except the one just pushed. This runs after the push so a
	// failed build/push never leaves the registry without a controller image.
	newDigest, err := output(ctx, "gcloud", "container", "images", "describe", o.image,
		"--format=value(image_summary.digest)")
	if err != nil {
		return err
	}
	allDigests, err := output(ctx, "gcloud", "container", "images", "list-tags", o.imageRepo, "--format=get(digest)")
	if err != nil {
		return err
	}
	oldDigests := filterDigests(allDigests, newDigest)
	if len(oldDigests) == 0 {
		logInfo("No older controller images to delete.")
		return nil
	}
	logInfo("Deleting older controller images from %s...", o.imageRepo)
	for _, digest := range oldDigests {
		if err := run(ctx, "gcloud", "container", "images", "delete", o.imageRepo+"@"+digest,
			"--force-delete-tags", "--quiet"); err != nil {
			return err
		}
	}
	logSuccess("Older controller images deleted.")
	return nil
}

func (o *syncOptions) configureSourceSecret(ctx context.Context) error {
	logInfo("Verifying source cluster connectivity...")
	if err := quiet(ctx, "kubectl", "--context="+o.sourceContext, "get", "--raw", "/version"); err != nil {
		return fmt.Errorf("failed to connect to source cluster using context %q; run: "+
			"gcloud container clusters get-credentials %s --location %s --project %s\n%w",
			o.sourceContext, o.SourceCluster, o.SourceLocation, o.Project, err)
	}
	logSuccess("Verified source cluster connection successfully!")

	logInfo("Looking up endpoint for source cluster '%s'...", o.SourceCluster)
	info, err := output(ctx, "gcloud", "container", "clusters", "describe", o.SourceCluster,
		"--location", o.SourceLocation, "--project", o.Project,
		"--format=value(endpoint,masterAuth.clusterCaCertificate)")
	if err != nil {
		return err
	}
	fields := strings.Fields(info)
	if len(fields) != 2 {
		return fmt.Errorf("unexpected endpoint/CA output for source cluster %q", o.SourceCluster)
	}
	kubeconfig := generateSourceKubeconfig(fields[0], fields[1])

	logInfo("Creating destination namespace '%s' on dest cluster if not exists...", o.Namespace)
	if err := o.kubectlApply(ctx, namespaceManifest(o.Namespace)); err != nil {
		return err
	}

	logInfo("Creating Kubeconfig Secret '%s' in namespace '%s' on dest cluster...", sourceSecretName, o.Namespace)
	if err := o.kubectlApply(ctx, secretManifest(sourceSecretName, o.Namespace, "kubeconfig", []byte(kubeconfig))); err != nil {
		return err
	}
	logSuccess("Secret '%s' successfully created in namespace '%s'!", sourceSecretName, o.Namespace)
	return nil
}

func (o *syncOptions) deployController(ctx context.Context) error {
	configDir := filepath.Join(o.moduleDir, "config", "default")
	logInfo("Rendering manifests from %s...", configDir)
	manifests, err := output(ctx, "kubectl", "kustomize", configDir)
	if err != nil {
		return err
	}
	rendered, err := substituteImage(manifests, imagePlaceholder, o.image)
	if err != nil {
		return err
	}

	logInfo("Applying CRD, RBAC and controller Deployment...")
	if err := o.kubectlApply(ctx, rendered); err != nil {
		return err
	}

	logInfo("Annotating ServiceAccount for Workload Identity...")
	if err := run(ctx, "kubectl", "--context="+o.destContext, "-n", controllerNamespace,
		"annotate", "serviceaccount", controllerKSA,
		"iam.gke.io/gcp-service-account="+o.gsaEmail, "--overwrite"); err != nil {
		return err
	}

	logInfo("Waiting for KRMSyncer CRD to be established...")
	if err := run(ctx, "kubectl", "--context="+o.destContext, "wait", "--for", "condition=established",
		"--timeout=60s", "crd/"+krmsyncerCRD); err != nil {
		return err
	}

	// Restart so pods pick up the ServiceAccount annotation and any newly pushed :latest image.
	logInfo("Restarting controller...")
	if err := run(ctx, "kubectl", "--context="+o.destContext, "-n", controllerNamespace,
		"rollout", "restart", "deployment", controllerDeployment); err != nil {
		return err
	}
	if err := run(ctx, "kubectl", "--context="+o.destContext, "-n", controllerNamespace,
		"rollout", "status", "deployment", controllerDeployment, "--timeout=180s"); err != nil {
		return err
	}
	logSuccess("KRMSyncer controller is running!")
	return nil
}

func (o *syncOptions) deploySyncConfig(ctx context.Context) error {
	logInfo("Deploying KRMSyncer sync configuration...")
	if err := o.kubectlApply(ctx, generateSyncConfig(o.Namespace)); err != nil {
		return err
	}
	logSuccess("Sample sync configuration applied successfully!")
	return nil
}

// kubectlApply applies the given manifests to the destination cluster.
func (o *syncOptions) kubectlApply(ctx context.Context, manifests string) error {
	return runWithStdin(ctx, manifests, "kubectl", "--context="+o.destContext, "apply", "-f", "-")
}
