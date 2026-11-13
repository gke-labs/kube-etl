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

import "strings"

// kccSystemGroup is the API group of Config Connector's own configuration
// (ConfigConnector, ConfigConnectorContext). Its subgroups hold the operator's
// controller customizations (customize.core.cnrm.cloud.google.com) and the
// multi-cluster leader election lease (multicluster.core.cnrm.cloud.google.com).
const kccSystemGroup = "core.cnrm.cloud.google.com"

// kccGlobMatchesGroup reports whether the "*.cnrm.cloud.google.com" glob
// matches group: a group of Config Connector resources.
//
// The glob skips kccSystemGroup and its subgroups. They hold each cluster's own
// Config Connector configuration and state, not Google Cloud resources: syncing
// them would overwrite the destination's ConfigConnector objects, controller
// customizations and leader election lease with the source's. A rule that names
// one of these groups explicitly still matches it.
func kccGlobMatchesGroup(group string) bool {
	if group == kccSystemGroup || strings.HasSuffix(group, "."+kccSystemGroup) {
		return false
	}
	return strings.HasSuffix(group, "cnrm.cloud.google.com")
}
