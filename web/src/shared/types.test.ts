import { describe, expect, it } from 'vitest';

import type { Agent } from './types.js';
import { agentUsesNvidiaGPU, isAgentPrivileged } from './types.js';

function agent(appliedConfig: Agent['appliedConfig']): Agent {
  return {
    id: 'agent-1',
    name: 'agent-1',
    projectId: 'project-1',
    template: 'developer',
    phase: 'running',
    appliedConfig,
  };
}

describe('agent runtime facts', () => {
  it('prefers observed privileged mode over the inline request', () => {
    expect(
      isAgentPrivileged(
        agent({
          docker: { privileged: true },
          inlineConfig: { docker: { privileged: false } },
        })
      )
    ).toBe(true);
  });

  it('falls back to inline privileged mode for older brokers', () => {
    expect(isAgentPrivileged(agent({ inlineConfig: { docker: { privileged: true } } }))).toBe(true);
  });

  it('prefers observed GPU attachment over the scheduling requirement', () => {
    expect(agentUsesNvidiaGPU(agent({ nvidiaGpu: false, requireGpu: true }))).toBe(false);
    expect(agentUsesNvidiaGPU(agent({ nvidiaGpu: true, requireGpu: false }))).toBe(true);
  });

  it('uses the GPU requirement as a compatibility fallback', () => {
    expect(agentUsesNvidiaGPU(agent({ requireGpu: true }))).toBe(true);
  });
});
