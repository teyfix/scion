/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import type { DockerRuntimeConfig } from './types.js';

export interface KeyValueEntry {
  key: string;
  value: string;
}

export const BUILT_IN_DOCKER_LABEL_VARIABLES = [
  'SCION_AGENT_NAME',
  'SCION_AGENT_SLUG',
  'SCION_AGENT_ID',
  'SCION_PROJECT',
  'SCION_PROJECT_SLUG',
  'SCION_PROJECT_ID',
] as const;

/** Build the explicit Docker override while preserving unrelated Docker fields. */
export function buildDockerRuntimeConfig(
  base: DockerRuntimeConfig | undefined,
  networks: string[],
  labels: KeyValueEntry[]
): DockerRuntimeConfig | undefined {
  const result: DockerRuntimeConfig = { ...base };

  const seenNetworks = new Set<string>();
  const normalizedNetworks: string[] = [];
  for (const network of networks) {
    const normalized = network.trim();
    if (normalized && !seenNetworks.has(normalized)) {
      seenNetworks.add(normalized);
      normalizedNetworks.push(normalized);
    }
  }
  if (normalizedNetworks.length > 0) result.networks = normalizedNetworks;
  else delete result.networks;

  const normalizedLabels: Record<string, string> = {};
  for (const label of labels) {
    const key = label.key.trim();
    if (key) normalizedLabels[key] = label.value.trim();
  }
  if (Object.keys(normalizedLabels).length > 0) result.labels = normalizedLabels;
  else delete result.labels;

  return Object.keys(result).length > 0 ? result : undefined;
}

/** Return referenced variables that are unavailable in the current creation scope. */
export function findMissingDockerLabelVariables(
  labels: KeyValueEntry[],
  availableVariables: Iterable<string>
): string[] {
  const available = new Set(availableVariables);
  const missing = new Set<string>();
  const variablePattern = /\$\{([^}]+)\}/g;

  for (const label of labels) {
    const candidate = `${label.key} ${label.value}`;
    for (const match of candidate.matchAll(variablePattern)) {
      if (!available.has(match[1])) missing.add(match[1]);
    }
  }

  return [...missing].sort();
}
