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

package controllers

import (
	"fmt"
	"slices"
	"strings"

	krmv1alpha1 "github.com/gke-labs/kube-etl/syncer/api/v1alpha1"
)

// The controller has cluster-wide access, but a KRMSyncer is a namespaced
// object. Without limits, anyone who can create a KRMSyncer in one namespace
// could make the controller read, write or delete objects in any other
// namespace on their behalf (a confused deputy): in pull mode with objects that
// come from a remote cluster they may control, in push mode by copying other
// namespaces' objects to a remote cluster.
//
// So every rule lists the namespaces it syncs, and a KRMSyncer only reaches
// outside its own namespace if the controller trusts the namespace it is in
// (KRMSyncerReconciler.TrustedNamespaces, the --trusted-namespaces flag). Only
// cluster administrators should be able to create KRMSyncers in trusted
// namespaces. Cluster-scoped objects are never synced.

// AllNamespaces in a rule's namespaces selects objects in every namespace. In
// the trusted namespaces it trusts every namespace.
const AllNamespaces = "*"

// Reasons of the InvalidRule condition of a KRMSyncer rejected because of its
// rules' namespaces or kinds.
const (
	// A rule's namespaces are missing or malformed.
	reasonInvalidNamespaces = "InvalidNamespaces"
	// Rules select namespaces outside the KRMSyncer's own, and its namespace
	// is not trusted.
	reasonNamespaceNotAllowed = "NamespaceNotAllowed"
	// A rule names a cluster-scoped kind.
	reasonClusterScopedKind = "ClusterScopedKind"
)

// ruleSelects reports whether rule selects objects in namespace, which is
// empty for cluster-scoped objects. Cluster-scoped objects are never selected.
func ruleSelects(rule krmv1alpha1.ResourceRule, namespace string) bool {
	if namespace == "" {
		return false
	}
	for _, ns := range rule.Namespaces {
		if ns == AllNamespaces || ns == namespace {
			return true
		}
	}
	return false
}

// namespaceTrusted reports whether KRMSyncers in namespace may select objects
// outside it.
func namespaceTrusted(namespace string, trusted []string) bool {
	for _, t := range trusted {
		if t == AllNamespaces || t == namespace {
			return true
		}
	}
	return false
}

// checkNamespaces returns an error, and the InvalidRule condition reason for
// it, if a rule of krmsyncer has missing or malformed namespaces, or if its
// rules select namespaces outside its own and its namespace is not trusted.
// Nothing is synced for a KRMSyncer with such an error.
//
// The CRD requires at least one non-empty namespace in new objects; objects
// stored before it did are rejected here.
func checkNamespaces(krmsyncer *krmv1alpha1.KRMSyncer, trusted []string) (reason string, err error) {
	for i, rule := range krmsyncer.Spec.Rules {
		field := fmt.Sprintf("spec.rules[%d].namespaces", i)
		switch {
		case len(rule.Namespaces) == 0:
			return reasonInvalidNamespaces, fmt.Errorf("%s is required: list the namespaces to sync, such as %q, or %q for every namespace",
				field, krmsyncer.Namespace, AllNamespaces)
		case slices.Contains(rule.Namespaces, ""):
			return reasonInvalidNamespaces, fmt.Errorf("%s must not contain empty names", field)
		case len(rule.Namespaces) > 1 && slices.Contains(rule.Namespaces, AllNamespaces):
			return reasonInvalidNamespaces, fmt.Errorf("%s: %q cannot be combined with other namespaces", field, AllNamespaces)
		}
	}
	if err := checkSyncerScope(krmsyncer, trusted); err != nil {
		return reasonNamespaceNotAllowed, err
	}
	return "", nil
}

// checkSyncerScope returns an error if krmsyncer has rules that select objects
// outside its own namespace (another namespace, or AllNamespaces) and its
// namespace is not trusted.
func checkSyncerScope(krmsyncer *krmv1alpha1.KRMSyncer, trusted []string) error {
	if namespaceTrusted(krmsyncer.Namespace, trusted) {
		return nil
	}
	var outside []string
	for _, rule := range krmsyncer.Spec.Rules {
		for _, ns := range rule.Namespaces {
			if ns != krmsyncer.Namespace && !slices.Contains(outside, ns) {
				outside = append(outside, ns)
			}
		}
	}
	if len(outside) == 0 {
		return nil
	}
	quoted := make([]string, len(outside))
	for i, ns := range outside {
		quoted[i] = fmt.Sprintf("%q", ns)
	}
	return fmt.Errorf("rules select namespaces outside the KRMSyncer's namespace %q (%s); only KRMSyncers in a namespace trusted by the controller (--trusted-namespaces) may do that. Limit the rules to %q, or move the KRMSyncer to a trusted namespace",
		krmsyncer.Namespace, strings.Join(quoted, ", "), krmsyncer.Namespace)
}
