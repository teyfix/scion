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

import { render } from 'lit';
import { describe, expect, it } from 'vitest';

import type { Agent, AgentInlineConfig, DockerRuntimeConfig } from '../../shared/types.js';
import { ScionPageAgentConfigure } from './agent-configure.js';
import { ScionPageAgentCreate } from './agent-create.js';
import { ScionPageAgentDetail } from './agent-detail.js';

interface TestableCreatePage {
  labelEntries: Array<{ key: string; value: string }>;
  dockerLabelEntries: Array<{ key: string; value: string }>;
  dockerNetworks: string[];
  dockerPrivileged: 'inherit' | 'enabled' | 'disabled';
  buildLabels(): Record<string, string> | undefined;
  buildConfig(): Record<string, unknown>;
}

interface TestableConfigurePage {
  dockerConfigBase: DockerRuntimeConfig | undefined;
  dockerLabelEntries: Array<{ key: string; value: string }>;
  dockerNetworks: string[];
  buildConfig(): { docker?: DockerRuntimeConfig };
}

interface TestableDetailPage {
  renderRuntimeCard(agent: Agent, inline: AgentInlineConfig | undefined): unknown;
}

describe('agent Docker configuration forms', () => {
  it('keeps searchable agent labels separate from Docker runtime labels', () => {
    const page = new ScionPageAgentCreate() as unknown as TestableCreatePage;
    page.labelEntries = [{ key: 'team', value: 'platform' }];
    page.dockerLabelEntries = [{ key: 'traefik.enable', value: 'true' }];
    page.dockerNetworks = ['traefik_proxy'];
    page.dockerPrivileged = 'inherit';

    expect(page.buildLabels()).toEqual({ team: 'platform' });
    expect(page.buildConfig().docker).toEqual({
      networks: ['traefik_proxy'],
      labels: { 'traefik.enable': 'true' },
    });
  });

  it('preserves privileged mode when a provisioned agent Docker config is edited', () => {
    const page = new ScionPageAgentConfigure() as unknown as TestableConfigurePage;
    page.dockerConfigBase = { privileged: true };
    page.dockerNetworks = ['traefik_proxy'];
    page.dockerLabelEntries = [{ key: 'route', value: '${SCION_AGENT_SLUG}' }];

    expect(page.buildConfig().docker).toEqual({
      privileged: true,
      networks: ['traefik_proxy'],
      labels: { route: '${SCION_AGENT_SLUG}' },
    });
  });

  it('shows configured networks and Docker labels in agent details', () => {
    const page = new ScionPageAgentDetail() as unknown as TestableDetailPage;
    const container = document.createElement('div');
    const agent = {
      id: 'agent-1',
      name: 'agent-1',
      projectId: 'project-1',
      template: 'default',
      phase: 'created',
    } as Agent;

    render(
      page.renderRuntimeCard(agent, {
        docker: {
          networks: ['traefik_proxy', 'backend'],
          labels: { 'traefik.enable': 'true' },
        },
      }),
      container
    );

    expect(container.textContent).toContain('Configured Docker Networks');
    expect(container.textContent).toContain('traefik_proxy');
    expect(container.textContent).toContain('backend');
    expect(container.textContent).toContain('Configured Docker Labels');
    expect(container.textContent).toContain('traefik.enable=true');
  });
});
