/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import { afterEach, describe, expect, it } from 'vitest';
import './status-badge.js';
import type { ScionStatusBadge } from './status-badge.js';

afterEach(() => {
  document.body.replaceChildren();
});

describe('status badge label DOM', () => {
  it('renders the default label as text without an extra element per badge', async () => {
    const badge = document.createElement('scion-status-badge') as ScionStatusBadge;
    badge.status = 'neutral';
    badge.showIcon = false;
    document.body.appendChild(badge);
    await badge.updateComplete;

    const content = badge.shadowRoot!.querySelector('.badge')!;
    expect(content.textContent?.trim()).toBe('neutral');
    expect(content.childElementCount).toBe(0);
    expect(badge.shadowRoot!.querySelector('slot')).toBeNull();
  });

  it('keeps custom GPU labels and icons through the existing reactive label property', async () => {
    const badge = document.createElement('scion-status-badge') as ScionStatusBadge;
    badge.setAttribute('status', 'neutral');
    badge.setAttribute('icon', 'gpu');
    badge.setAttribute('label', 'GPU');
    document.body.appendChild(badge);
    await badge.updateComplete;

    const content = badge.shadowRoot!.querySelector('.badge')!;
    expect(content.textContent?.trim()).toBe('GPU');
    expect(content.childElementCount).toBe(1);
    expect(content.querySelector('sl-icon')?.getAttribute('name')).toBe('gpu-card');
    expect(badge.shadowRoot!.querySelector('slot')).toBeNull();

    badge.label = 'GPU reserved';
    await badge.updateComplete;
    expect(content.textContent?.trim()).toBe('GPU reserved');
    expect(content.childElementCount).toBe(1);
  });
});
