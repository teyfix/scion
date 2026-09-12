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

package agent

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
)

// BuildScopedLabelVars constructs the scoped variable map for dynamic label expansion.
// It sets authoritative SCION identity variables and ingests explicitly provided environment
// variables (such as APP_DOMAIN). For keys with an empty value, it resolves them via host
// environment passthrough (os.LookupEnv). Ambient host environment variables not explicitly listed
// in env are excluded.
func BuildScopedLabelVars(agentName, agentID, projectName, projectID string, env map[string]string) map[string]string {
	vars := make(map[string]string)

	// Ingest explicitly declared environment variables.
	for k, v := range env {
		if v == "" {
			// Empty value = explicit passthrough allowlist: resolve from host env.
			if hostVal, ok := os.LookupEnv(k); ok && hostVal != "" {
				vars[k] = hostVal
			}
			// If not set on host, leave absent — ExpandAndValidateDockerLabels
			// will fail with "unresolved variable ${K}" if referenced in a label.
		} else {
			vars[k] = v
		}
	}

	// Authoritative SCION identity variables take precedence over config env.
	vars["SCION_AGENT_NAME"] = agentName
	vars["SCION_AGENT_SLUG"] = api.Slugify(agentName)
	vars["SCION_AGENT_ID"] = agentID
	vars["SCION_PROJECT"] = projectName
	vars["SCION_PROJECT_SLUG"] = api.Slugify(projectName)
	vars["SCION_PROJECT_ID"] = projectID

	return vars
}

// ExpandAndValidateDockerLabels expands ${VAR} placeholders in both keys and values,
// validates that all referenced variables exist, detects key collisions, and rejects
// collisions with reserved internal SCION labels.
func ExpandAndValidateDockerLabels(labels map[string]string, vars map[string]string) (map[string]string, error) {
	if len(labels) == 0 {
		return nil, nil
	}

	// Sort input keys for deterministic processing and error reporting.
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	resolved := make(map[string]string, len(labels))
	seenExpandedKeys := make(map[string]string, len(labels)) // expandedKey -> originalKey

	for _, originalKey := range keys {
		originalVal := labels[originalKey]

		expandedKey, err := expandScopedVars(originalKey, vars)
		if err != nil {
			return nil, fmt.Errorf("invalid label key %q: %w", originalKey, err)
		}
		if expandedKey == "" {
			return nil, fmt.Errorf("label key %q expanded to empty string", originalKey)
		}

		if projectcompat.IsReservedInternalLabel(expandedKey) {
			return nil, fmt.Errorf("label key %q (expanded: %q) collides with reserved internal label", originalKey, expandedKey)
		}

		if priorOriginal, exists := seenExpandedKeys[expandedKey]; exists {
			return nil, fmt.Errorf("docker label key collision: %q and %q both expand to %q", priorOriginal, originalKey, expandedKey)
		}

		expandedVal, err := expandScopedVars(originalVal, vars)
		if err != nil {
			return nil, fmt.Errorf("invalid label value for key %q: %w", originalKey, err)
		}

		seenExpandedKeys[expandedKey] = originalKey
		resolved[expandedKey] = expandedVal
	}

	return resolved, nil
}

// expandScopedVars replaces all ${VAR} patterns in input with corresponding values from vars.
// If any ${VAR} reference is unclosed or not present in vars, an error is returned.
func expandScopedVars(input string, vars map[string]string) (string, error) {
	var sb strings.Builder
	remaining := input

	for {
		start := strings.Index(remaining, "${")
		if start == -1 {
			sb.WriteString(remaining)
			break
		}

		sb.WriteString(remaining[:start])
		remaining = remaining[start+2:]

		end := strings.IndexByte(remaining, '}')
		if end == -1 {
			return "", fmt.Errorf("unclosed variable reference ${%s in %q", remaining, input)
		}

		varName := remaining[:end]
		remaining = remaining[end+1:]

		if varName == "" {
			return "", fmt.Errorf("empty variable reference ${} in %q", input)
		}

		val, ok := vars[varName]
		if !ok {
			return "", fmt.Errorf("unresolved variable ${%s}", varName)
		}

		sb.WriteString(val)
	}

	return sb.String(), nil
}
