package export

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gke-labs/kube-etl/pkg/sink"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

type ExportOptions struct {
	Output string
}

func (o *ExportOptions) InitDefaults() {
	// No defaults for now
}

func BuildExportCommand() *cobra.Command {
	var opt ExportOptions
	opt.InitDefaults()

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export Kubernetes objects from the current cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			if opt.Output == "" {
				return fmt.Errorf("required flag(s) \"output\" not set")
			}
			return Run(cmd.Context(), opt)
		},
	}

	cmd.Flags().StringVar(&opt.Output, "output", opt.Output, "Path to the output file (e.g. output.zip)")

	return cmd
}

func Run(ctx context.Context, opt ExportOptions) error {
	s, err := sink.NewZipSink(opt.Output)
	if err != nil {
		return fmt.Errorf("failed to create sink: %w", err)
	}
	defer s.Close()

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	config, err := kubeConfig.ClientConfig()
	if err != nil {
		return fmt.Errorf("failed to get k8s config: %w", err)
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create discovery client: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create dynamic client: %w", err)
	}

	lists, err := discoveryClient.ServerPreferredResources()
	if err != nil {
		if !discovery.IsGroupDiscoveryFailedError(err) {
			// If it's a critical error, we might want to fail, but often discovery has partial errors.
			// We'll proceed with what we have if lists is not empty.
			if len(lists) == 0 {
				return fmt.Errorf("failed to discover resources: %w", err)
			}
			fmt.Printf("Warning: partial discovery error: %v\n", err)
		}
	}

	var errs []error

	for _, list := range lists {
		groupVersion, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to parse group version %q: %w", list.GroupVersion, err))
			continue
		}

		for _, resource := range list.APIResources {
			if !contains(resource.Verbs, "list") {
				continue
			}

			// Skip subresources
			if strings.Contains(resource.Name, "/") {
				continue
			}

			gvr := groupVersion.WithResource(resource.Name)

			uList, err := dynamicClient.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to list %s: %w", gvr, err))
				continue
			}

			for _, item := range uList.Items {
				ns := item.GetNamespace()
				if ns == "" {
					ns = "_cluster"
				}

				group := gvr.Group
				if group == "" {
					group = "core"
				}

				kind := item.GetKind()
				name := item.GetName()

				path := filepath.Join(ns, group, kind, name+".yaml")

				data, err := yaml.Marshal(item.Object)
				if err != nil {
					errs = append(errs, fmt.Errorf("failed to marshal %s/%s: %w", kind, name, err))
					continue
				}

				if err := s.Write(path, data); err != nil {
					// writing to sink failure is probably fatal for that file, but maybe not for the whole process?
					// The sink might be broken though.
					errs = append(errs, fmt.Errorf("failed to write %s to sink: %w", path, err))
				}
			}
		}
	}

	return errors.Join(errs...)
}

func contains(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}