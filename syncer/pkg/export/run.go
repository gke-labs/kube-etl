package export

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gke-labs/kube-etl/pkg/sink"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

func Run(outputPath string) error {
	s, err := sink.NewZipSink(outputPath)
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

	for _, list := range lists {
		groupVersion, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
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
            
			uList, err := dynamicClient.Resource(gvr).Namespace(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
			if err != nil {
                // Some resources might be forbidden or not listable for other reasons.
                // We shouldn't stop the whole export.
				fmt.Printf("Warning: failed to list %s: %v\n", gvr, err)
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
					fmt.Printf("Warning: failed to marshal %s/%s: %v\n", kind, name, err)
					continue
				}

				if err := s.Write(path, data); err != nil {
					return fmt.Errorf("failed to write to sink: %w", err)
				}
			}
		}
	}

	return nil
}

func contains(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}
