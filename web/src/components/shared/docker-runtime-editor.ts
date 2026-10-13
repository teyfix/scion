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

import { html } from 'lit';
import type { DockerRuntimeConfig } from '../../shared/types.js';
import type { KeyValueEntry } from '../../shared/docker-runtime.js';

export interface DockerEditorState {
  privileged: 'inherit' | 'enabled' | 'disabled';
  gpu: boolean | undefined;
  networks: string[];
  devices: string[];
  labels: KeyValueEntry[];
}

/** Runtime controls, separate from searchable agent metadata labels. */
export function renderDockerRuntimeEditor(
  state: DockerEditorState,
  change: (next: DockerEditorState) => void
) {
  const text = (event: Event) => (event.target as HTMLElement & { value: string }).value;
  return html`<div class="form-section">
    <h3>Docker Runtime</h3>
    <div class="form-field">
      <label>Privileged</label>
      <sl-select
        .value=${state.privileged}
        @sl-change=${(e: Event) =>
          change({ ...state, privileged: text(e) as DockerEditorState['privileged'] })}
      >
        <sl-option value="inherit">Inherit</sl-option><sl-option value="enabled">Enabled</sl-option
        ><sl-option value="disabled">Disabled</sl-option>
      </sl-select>
      <div class="hint">An explicit Disabled overrides inherited privileged mode.</div>
    </div>
    <div class="form-field">
      <sl-checkbox
        .checked=${state.gpu === true}
        .indeterminate=${state.gpu === undefined}
        @sl-change=${(e: Event) =>
          change({ ...state, gpu: (e.target as HTMLElement & { checked: boolean }).checked })}
        >Attach NVIDIA GPU</sl-checkbox
      >
      <sl-button
        size="small"
        variant="text"
        aria-label="Inherit GPU settings"
        ?disabled=${state.gpu === undefined}
        @click=${() => change({ ...state, gpu: undefined })}
        >Inherit</sl-button
      >
      <div class="hint">
        ${state.gpu === undefined
          ? 'Inherit GPU settings. Changing the checkbox records an explicit attachment choice.'
          : state.gpu
            ? 'Attach configured NVIDIA devices, or nvidia.com/gpu=all. Requires a GPU-capable broker.'
            : 'Disable NVIDIA device grants, including inherited devices.'}
      </div>
    </div>
    <div class="form-field">
      <sl-textarea
        label="Docker Networks"
        .value=${state.networks.join('\n')}
        @sl-input=${(e: Event) => change({ ...state, networks: text(e).split('\n') })}
        rows="2"
      ></sl-textarea>
      <div class="hint">One network per line. Inherited required networks remain attached.</div>
    </div>
    <div class="form-field">
      <sl-textarea
        label="Docker Devices"
        .value=${state.devices.join('\n')}
        @sl-input=${(e: Event) =>
          change({
            ...state,
            devices: text(e)
              .split('\n')
              .map((v) => v.trim())
              .filter(Boolean),
          })}
        rows="2"
      ></sl-textarea>
      <div class="hint">
        One host device path or CDI selector per line, for example nvidia.com/gpu=all.
      </div>
    </div>
    <div class="form-field">
      <sl-textarea
        label="Docker Labels"
        .value=${state.labels.map((l) => l.key + '=' + l.value).join('\n')}
        @sl-input=${(e: Event) =>
          change({
            ...state,
            labels: text(e)
              .split('\n')
              .filter(Boolean)
              .map((line) => {
                const i = line.indexOf('=');
                return {
                  key: i < 0 ? line : line.slice(0, i),
                  value: i < 0 ? '' : line.slice(i + 1),
                };
              }),
          })}
        rows="3"
      ></sl-textarea>
      <div class="hint">
        One key=value per line. Supports scoped variables such as \${SCION_AGENT_SLUG}. The scion.*
        prefix is reserved. These labels configure the container.
      </div>
    </div>
  </div>`;
}

export function editorPrivileged(
  config: DockerRuntimeConfig | undefined
): DockerEditorState['privileged'] {
  return config?.privileged === undefined ? 'inherit' : config.privileged ? 'enabled' : 'disabled';
}
