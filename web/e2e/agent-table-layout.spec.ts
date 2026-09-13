// Copyright 2026 Google LLC
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//     http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import { test, expect } from '@playwright/test';
import { getE2EEnv } from './harness/env.js';

const env = getE2EEnv();
test.use({ baseURL: env.baseURL });

const agents = ['animatrix-coordinator', 'animatrix-issue-369-registration'].map((name, i) => ({
  id: `layout-agent-${i}`,
  name,
  projectId: 'layout-project',
  project: 'animatrix',
  template: 'codex-astra-high',
  phase: 'running',
  activity: 'thinking',
  taskSummary: 'Stabilize broker registration and issue lifecycle for product features',
  updated: new Date().toISOString(),
  messageMode: 'project',
  runtimeBrokerId: 'layout-broker',
  runtimeBrokerName: 'wsl2-gpu-broker',
  _capabilities: { actions: ['attach', 'delete'] },
  appliedConfig: { docker: { privileged: true }, nvidiaGpu: true },
}));

for (const path of ['/agents', '/projects/layout-project']) {
  for (const viewport of [
    { width: 1917, height: 906 },
    { width: 1024, height: 768 },
  ]) {
    test(`${path} keeps names, capabilities and actions usable at ${viewport.width}px`, async ({
      page,
    }, testInfo) => {
      await page.setViewportSize(viewport);
      await page.addInitScript(() => {
        localStorage.setItem('scion-view-agents', 'list');
        localStorage.setItem('scion-view-project-agents', 'list');
      });
      await page.route('**/auth/me', (route) =>
        route.fulfill({
          json: {
            id: 'layout-user',
            email: 'layout@example.test',
            displayName: 'Layout test',
            role: 'admin',
          },
        })
      );
      await page.route('**/api/v1/**', async (route) => {
        const url = new URL(route.request().url());
        let json: unknown = [];
        if (url.pathname === '/api/v1/system/status') json = { complete: true };
        else if (
          url.pathname === '/api/v1/agents' ||
          url.pathname === '/api/v1/projects/layout-project/agents'
        ) {
          json = { agents, _capabilities: { actions: [] } };
        } else if (url.pathname === '/api/v1/projects/layout-project') {
          json = {
            id: 'layout-project',
            name: 'animatrix',
            gitRemote: 'https://example.test/repo.git',
          };
        } else if (url.pathname.endsWith('/metrics/summary')) {
          json = { totalSessions: 12, totalTokensInput: 1234567, totalTokensOutput: 234567 };
        } else if (url.pathname.endsWith('/metrics-summary')) json = { available: false };
        await route.fulfill({ json });
      });

      await page.goto(path, { waitUntil: 'domcontentloaded' });
      const table = page.locator('.resource-table-container, .agent-table-container');
      const name = table.locator('.name-cell a').first();
      await expect(name).toHaveText(agents[0].name);
      await expect(table.locator('.name-cell').first()).toContainText('Privileged');
      await expect(table.locator('.name-cell').first()).toContainText('GPU');
      await expect(table.locator('sl-button[aria-label="Terminal"]').first()).toBeVisible();
      await expect(table.locator('sl-button[aria-label="Stop"]').first()).toBeVisible();

      const geometry = await name.evaluate((link) => {
        const range = document.createRange();
        range.selectNodeContents(link);
        return {
          lines: range.getClientRects().length,
          width: link.clientWidth,
          scrollWidth: link.scrollWidth,
        };
      });
      expect(geometry.lines).toBe(1);
      expect(geometry.scrollWidth).toBeLessThanOrEqual(geometry.width);
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(
        viewport.width
      );
      const boundary = await table.evaluate((container) => ({
        width: container.clientWidth,
        scrollWidth: container.scrollWidth,
      }));
      if (viewport.width === 1917) expect(boundary.scrollWidth).toBeLessThanOrEqual(boundary.width);

      await table.evaluate((container) => {
        container.scrollLeft = container.scrollWidth;
      });
      const action = table.locator('tbody tr').first().locator('sl-button').last();
      const actionBox = await action.boundingBox();
      const tableBox = await table.boundingBox();
      expect(actionBox).not.toBeNull();
      expect(tableBox).not.toBeNull();
      if (actionBox && tableBox) {
        expect(actionBox.x).toBeGreaterThanOrEqual(tableBox.x);
        expect(actionBox.x + actionBox.width).toBeLessThanOrEqual(tableBox.x + tableBox.width);
      }
      await table.evaluate((container) => {
        container.scrollLeft = 0;
      });
      await testInfo.attach('agent-table-layout', {
        body: await page.screenshot(),
        contentType: 'image/png',
      });
    });
  }
}
