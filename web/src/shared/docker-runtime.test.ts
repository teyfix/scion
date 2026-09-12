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

import { describe, expect, it } from 'vitest';

import { buildDockerRuntimeConfig, findMissingDockerLabelVariables } from './docker-runtime.js';

describe('buildDockerRuntimeConfig', () => {
  it('normalizes networks and labels while preserving other Docker fields', () => {
    expect(
      buildDockerRuntimeConfig(
        { privileged: true },
        [' traefik_proxy ', 'backend', 'traefik_proxy', ''],
        [
          { key: ' traefik.enable ', value: ' true ' },
          { key: '', value: 'ignored' },
        ]
      )
    ).toEqual({
      privileged: true,
      networks: ['traefik_proxy', 'backend'],
      labels: { 'traefik.enable': 'true' },
    });
  });

  it('removes cleared network and label overrides without dropping privileged', () => {
    expect(
      buildDockerRuntimeConfig(
        { privileged: false, networks: ['old'], labels: { old: 'value' } },
        [],
        []
      )
    ).toEqual({ privileged: false });
  });

  it('returns undefined when no Docker override remains', () => {
    expect(buildDockerRuntimeConfig(undefined, [' '], [{ key: '', value: '' }])).toBeUndefined();
  });
});

describe('findMissingDockerLabelVariables', () => {
  it('checks keys and values and returns stable unique names', () => {
    const missing = findMissingDockerLabelVariables(
      [
        { key: 'router.${ROUTE}', value: '${APP_DOMAIN}' },
        { key: 'again', value: '${ROUTE}-${SCION_AGENT_SLUG}' },
      ],
      ['SCION_AGENT_SLUG']
    );

    expect(missing).toEqual(['APP_DOMAIN', 'ROUTE']);
  });
});
