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
  loading: boolean;
  render(): unknown;
  labelEntries: Array<{ key: string; value: string }>;
  dockerLabelEntries: Array<{ key: string; value: string }>;
  dockerNetworks: string[];
  dockerPrivileged: 'inherit' | 'enabled' | 'disabled';
  requireGpu: boolean | undefined;
  buildDockerConfig(): Record<string, unknown>;
}

interface TestableConfigurePage {
  dockerConfigBase: DockerRuntimeConfig | undefined;
  dockerLabelEntries: Array<{ key: string; value: string }>;
  dockerNetworks: string[];
  buildConfig(): { docker?: DockerRuntimeConfig };
  renderEnvironmentTab(): unknown;
}

interface TestableDetailPage {
  renderRuntimeCard(agent: Agent, inline: AgentInlineConfig | undefined): unknown;
}

describe('agent Docker configuration forms', () => {
  it('builds runtime labels and required networks separately from agent metadata', () => {
    const page = new ScionPageAgentCreate() as unknown as TestableCreatePage;
    page.dockerLabelEntries = [{ key: 'traefik.enable', value: 'true' }];
    page.dockerNetworks = ['traefik_proxy'];
    expect(page.buildDockerConfig().docker).toEqual({
      networks: ['traefik_proxy'],
      labels: { 'traefik.enable': 'true' },
    });
  });

  it('omits untouched NVIDIA settings and sends explicit true or false', () => {
    const page = new ScionPageAgentCreate() as unknown as TestableCreatePage;
    expect(page.buildDockerConfig()).toEqual({});
    page.requireGpu = true;
    expect(page.buildDockerConfig().docker).toEqual({ nvidia_gpu: true });
    page.requireGpu = false;
    expect(page.buildDockerConfig().docker).toEqual({ nvidia_gpu: false });
  });

  it('does not pin GPU settings when another runtime control changes', () => {
    const page = new ScionPageAgentCreate() as unknown as TestableCreatePage;
    page.loading = false;
    const container = document.createElement('div');
    render(page.render(), container);
    const networks = container.querySelector<HTMLElement & { value: string }>(
      'sl-textarea[label="Docker Networks"]'
    );
    expect(networks).not.toBeNull();
    networks!.value = 'backend';
    networks!.dispatchEvent(new Event('sl-input'));
    expect(page.buildDockerConfig().docker).toEqual({ networks: ['backend'] });
  });

  it('returns to inherited GPU settings without discarding other Docker controls', () => {
    const page = new ScionPageAgentCreate() as unknown as TestableCreatePage;
    page.dockerNetworks = ['backend'];
    page.requireGpu = false;
    expect(page.buildDockerConfig().docker).toEqual({ nvidia_gpu: false, networks: ['backend'] });
    page.requireGpu = undefined;
    expect(page.buildDockerConfig().docker).toEqual({ networks: ['backend'] });
  });

  it('preserves omitted and explicit GPU values when editing a provisioned agent', () => {
    const page = new ScionPageAgentConfigure() as unknown as TestableConfigurePage;
    expect(page.buildConfig()).not.toHaveProperty('docker');
    for (const policy of [false, true]) {
      page.dockerConfigBase = { nvidia_gpu: policy };
      expect(page.buildConfig().docker).toEqual({ nvidia_gpu: policy });
    }
  });

  it('clears an edited GPU override through the Inherit control', () => {
    const page = new ScionPageAgentConfigure() as unknown as TestableConfigurePage;
    page.dockerConfigBase = { nvidia_gpu: false, privileged: false };
    page.dockerNetworks = ['backend'];
    const container = document.createElement('div');
    render(page.renderEnvironmentTab(), container);
    const inherit = container.querySelector<HTMLElement>(
      'sl-button[aria-label="Inherit GPU settings"]'
    );
    expect(inherit).not.toBeNull();
    inherit!.click();
    expect(page.buildConfig().docker).toEqual({ privileged: false, networks: ['backend'] });
    page.dockerConfigBase = { nvidia_gpu: true };
    page.dockerNetworks = [];
    render(page.renderEnvironmentTab(), container);
    container.querySelector<HTMLElement>('sl-button[aria-label="Inherit GPU settings"]')!.click();
    expect(page.buildConfig().docker).toEqual({});
  });

  it('preserves unrelated Docker fields when a provisioned agent config is edited', () => {
    const page = new ScionPageAgentConfigure() as unknown as TestableConfigurePage;
    page.dockerConfigBase = { privileged: true, devices: ['nvidia.com/gpu=all'] };
    page.dockerNetworks = ['traefik_proxy'];
    page.dockerLabelEntries = [{ key: 'route', value: '${SCION_AGENT_SLUG}' }];

    expect(page.buildConfig().docker).toEqual({
      privileged: true,
      devices: ['nvidia.com/gpu=all'],
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
          devices: ['nvidia.com/gpu=all'],
          labels: { 'traefik.enable': 'true' },
        },
      }),
      container
    );

    expect(container.textContent).toContain('Configured Docker Networks');
    expect(container.textContent).toContain('traefik_proxy');
    expect(container.textContent).toContain('backend');
    expect(container.textContent).toContain('Configured Docker Devices');
    expect(container.textContent).toContain('nvidia.com/gpu=all');
    expect(container.textContent).toContain('Configured Docker Labels');
    expect(container.textContent).toContain('traefik.enable=true');
  });
});
