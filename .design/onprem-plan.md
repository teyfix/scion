# Teyfix on-prem plan

Reconciled on 2026-09-10 against the requested Animatrix integration and the
fork at `7173b2aadb256cabcbee491f48beaee4106beee2`. This is a requirements and
acceptance checklist; unchecked items are not claims of implemented support.

Each milestone delivers a user-visible capability end to end: implementation,
focused tests, the minimum affected images, a published on-prem tag, deployment
verification, and removal of the corresponding Animatrix workaround. Building
an image is release transport, not the feature contract. Branch pushes publish
the current Animatrix artifact set; a manual `all` scope refreshes the complete
stock harness catalog.

## Ownership and packaging

Images provide executables, libraries, browser assets, and system packages.
Project behavior is supplied through SCION resources. Keep Animatrix scripts,
custom certificates, credentials, templates, MCP definitions, provider settings,
and repository contents out of the reusable images. SCION's embedded defaults
and dashboard remain normal application assets.

| Concern | Owner |
| --- | --- |
| Instructions, MCP definitions, resources, services, skills | Templates |
| Harness commands, auth mapping, OpenCode status plugin | Harness-config bundles, including `home/` |
| Provider credentials | User/project/Runtime Broker secrets |
| Project setup, CA activation, Git credential setup, Buildx registration | Hooks with appropriate startup ordering |
| Certificate/config mounts, BuildKit sockets, model directories | Runtime Broker profiles |
| Agent state notifications | Existing lifecycle webhooks and status/message APIs |
| Docker daemon, CLIs, system packages | Images |
| GitHub reconciliation, broker-selection policy, routing, DNS, TLS, Tailscale | Animatrix/deployment configuration |

Project bootstrap uses a one-shot CLI container. The Hub image does not need
Animatrix bootstrap tools. SCION supervises template-declared services; the
template declares how to launch `dockerd` using tools installed in the image.

## MS0 — Image delivery

- [x] `build-onprem.yml`: manual dispatch and pushes to `teyfix/onprem`;
  one active workflow run with `cancel-in-progress: true`.
- [x] GitHub-hosted linux/amd64 builds, parallel harness matrix, per-image
  GitHub Actions cache with intermediate stages, and native `GITHUB_TOKEN`.
- [x] Root Dockerfile embeds the dashboard; base/tool and Hub builds use
  `CGO_ENABLED=0`, `-trimpath`, and stripped linker flags.
- [x] Tag configuration: requested human tag (default `onprem-<6-char SHA>`),
  git short-commit tag, `sha-<40-char fork SHA>`, and optional `latest`.
  Release promotion uses the corrected Hub image and fork OCI metadata.
- [ ] Complete the live build and verify published manifests, cache export,
  dashboard, and binary contracts. Workflow implementation alone is not proof.
- [ ] Publish `scion-broker`: scion, Docker CLI/Buildx, gh, CA utilities,
  and Git >= 2.47.
- [ ] Publish `scion-onprem-agent`: sciontool as PID 1, scion user, Codex,
  Antigravity, OpenCode, Docker daemon/CLI/Compose/Buildx, Git, gh, Bun, Task,
  Python, uv/uvx, fish, tmux, btop, pstree, ripgrep, Chromium, browser MCP tools.
- [ ] Reuse the Omni harness installation chain with an amd64 base and agent
  startup. The existing `omni` target is amd64-only, adds a combined
  Hub/Runtime Broker command, and is excluded from `resolve_targets all`.
- [ ] Extend cache, dependency ordering, tag promotion, and smoke checks to
  both new images without depending on unpublished intermediate images.

Retain `scion-base` with `/usr/local/bin/scion` and `/usr/local/bin/sciontool`.
`scion-hub` provides the embedded-dashboard scion binary; it does not currently
provide sciontool. Keep existing base/harness outputs. Fork synchronization and
upstream contribution remain separate from image publishing.

## MS1 — Remote resources

- [ ] Fix harness-config local-storage download URL rewriting and raw HTTP
- [x] Fix harness-config local-storage download URL rewriting and raw HTTP
  file serving; inspect upload paths used by remote resource publishing too.
- [ ] Verify authenticated downloads through the advertised HTTP(S) Hub URL,
- [x] Verify authenticated downloads through the advertised HTTP(S) Hub URL,
  including reverse-proxy paths, escaped filenames, hash checking, and caching.
- [ ] Cover harness Dockerfiles, provisioners, supporting files, templates,
- [x] Cover harness Dockerfiles, provisioners, supporting files, templates,
  skills, and configuration without opening Hub filesystem paths on a Runtime
  Broker or requiring shared storage.
- [ ] Verify create, start/restart, and recreation with cold and warm caches.
- [x] Verify create, start/restart, and recreation with cold and warm caches.

Template and skill local-storage URL rewriting already exists; harness-config
rewriting is missing in this fork. Upstream issue #1235 concerns a related
restart hydration defect, not the identical file-URL bug. Reuse existing
hydration, transfer authentication, and cache implementations.
rewriting and remote hydration are implemented via the Hub authenticated download
handlers and runtime broker hydration cache.

## MS2 — Docker execution, networks, and runtime labels

- [x] Carry privileged mode and CDI requests such as `nvidia.com/gpu=all`
  through configuration, Hub applied config, dispatch, and Docker execution.
  (Privileged mode tri-state UI/API/dispatch/Docker execution and NvidiaGPU
  broker requirement matching completed; Docker CDI/device flag forwarding
  completed via `docker.devices`.)
- [ ] Verify existing environment, volumes, resources, and user handling;
  complete required capability, security-option, and network transport
  without silently dropping configured options or introducing WSL paths.
- [ ] Add per-service `user: root` and `required: true`. A required service's
  startup/readiness failure or permanent runtime failure must fail the agent.
- [ ] Provide dockerd readiness checking suitable for its Unix socket and
  gate dependent initialization/harness launch on readiness.
- [ ] Resolve startup dependencies: CA/auth needed for initial fetch/clone
  must exist before that operation; Buildx setup requiring nested Docker must
  happen after dockerd readiness. Today cloning precedes pre-start hooks and
  services start after those hooks.
- [x] Implement the following external-network and runtime-label contract.

### External networks and runtime labels

SCION's responsibility ends at container creation and lifecycle. Runtime
Brokers apply configured networks and labels to outer agent containers.
Traefik discovery, route interpretation, certificates, and peer access are
deployment responsibilities.

1. Runtime Broker profiles declare existing host Docker networks, for example
   `traefik_proxy`. Each agent using that profile must join the configured
   networks. If any required network is absent, fail creation visibly and
   propagate the Docker error to the Hub. Do not create it or fall back.
   Explicit network configuration takes precedence over automatic host-network
   selection; incompatible configurations must fail clearly.
2. Keep Docker runtime labels separate from searchable SCION agent labels.
   Runtime labels need their own transport and persistence; SCION agent label
   limits (16 entries, 63-character keys/values) are unsuitable for routing
   configuration. Preserve SCION's internal container identity/ownership labels.
   Reject user label collisions with those internal keys after expansion.
3. Merge profile defaults, template configuration, and explicit create
   overrides deterministically. Retain required profile network attachments;
   specify and test field-level precedence when implementing the schema.
4. Expand dynamic references in both runtime label keys and values after
   resolving agent identity and configured environment. Required examples are
   `${SCION_AGENT_SLUG}`, `${SCION_AGENT_ID}`, `${SCION_PROJECT_SLUG}`, and
   explicitly supplied non-secret `${APP_DOMAIN}`. Use a scoped resolver, not
   ambient host environment lookup or shell evaluation. Missing variables or
   expanded-key collisions must be reported rather than silently misrouting.
5. Pass resolved labels as individual Docker arguments. Persist the resolved
   runtime specification so ordinary restart/recreation reproduces it, even if
   profile defaults or the Runtime Broker environment subsequently change.
6. Dynamic means resolved at creation: Docker container labels cannot be
   edited in place. Applying changed labels requires deliberate recreation,
   preserving the agent identity, worktree, home, and managed Docker storage.

The transcript's `runtime.docker.networks` and `runtime.docker.labels` spellings
are proposed schema, not currently supported configuration. Domain examples are
`s3.{issue-slug}.{APP_DOMAIN}`, `studio.{issue-slug}.{APP_DOMAIN}`, and
`api.{issue-slug}.{APP_DOMAIN}`. Animatrix supplies the issue/agent naming and
label templates; SCION provides generic expansion. Do not assume an agent slug
equals an issue slug unless the caller establishes that mapping.

Low-level `RunConfig.Labels` already emits Docker `--label` arguments and
`RunConfig.NetworkMode` emits `--network`. The missing work is configuration,
end-to-end forwarding, scoped expansion, and resolved-spec persistence.

For DinD, inner service containers keep their private Docker networks. A port
published by nested Docker onto a reachable interface of the outer agent can
be reached over that agent's configured host network. Inner containers do not
join the host's `traefik_proxy` network. Application traffic need not pass
through the Hub or SCION proxy.

Acceptance checks: inspect outer-container network memberships and expanded
labels; reject a missing network or unresolved variable with a visible agent
failure; round-trip the applied specification; recreate without losing state;
verify the runtime can reach a DinD-published port. Traefik behavior is outside
these SCION acceptance checks.

## MS3 — Agent-owned persistent storage

- [ ] Mount an agent-owned managed Docker volume at `/var/lib/docker` using
  stable agent identity, preserving it across stop/start and recreation.
- [ ] Remove owned storage only on permanent agent deletion, including when
  the outer container has already disappeared; leave unrelated volumes alone.
- [ ] Handle root-owned nested-Docker data without recursive ownership changes
  or broker filesystem deletion workarounds.

## MS4 — Fresh workspaces and Runtime Broker discovery

- [ ] Before creating a new issue branch, fetch the configured default branch
  and create from the refreshed remote reference; do not hardcode `main`.
- [ ] Add strict worktree mode that fails visibly instead of falling back to
  clone-per-agent. Preserve existing branches and user work on restart.
- [ ] Add Runtime Broker labels in settings/environment and republish them
  on registration/reconnection: capability, capacity, harness availability,
  and operator-provided name.
  and operator-provided name. (NvidiaGPU capability reporting, CLI broker
  registration, and Hub DB mapping completed).
- [ ] Verify existing searchable agent labels, persistence, online/connected
  status, active-agent count, and explicit Runtime Broker selection.

Scheduling policy stays in Animatrix. Example labels include
`animatrix.capability.nvidia=true`, `animatrix.capacity.agents=4`, and
`animatrix.broker.name=...`.

## MS5 — Hub branding

- [ ] Provide logo URL, title, and subtitle through config/environment with
  sensible defaults, and verify them in the embedded production dashboard.

## MS6 — Resource migration and acceptance

- [ ] Publish harness-configs and templates; put the OpenCode status plugin
  in the harness-config `home/` layer, and project setup in appropriate hooks.
- [ ] Verify literal model selection, including `nvidia/moonshotai/kimi-k3`.
- [ ] Verify working, waiting, blocked, error, and completed-turn reporting
  through status/message APIs. Existing lifecycle webhooks cover running,
  stopped, suspended, and error; those events do not replace turn/activity APIs.
- [ ] Use one-shot CLI bootstrap and separate Hub/Runtime Broker processes.
- [ ] Verify secrets, skills, MCP translation, profile mounts, and existing
  SCION port forwarding where used.
- [ ] Remove each Animatrix workaround only after its replacement passes
  integration checks, including issue creation, remote hydration, DinD startup,
  external-network attachment, recreation, completion, and permanent deletion.

## Evidence and references

- [Build graph](../image-build/scripts/lib/targets.sh),
  [platform restrictions](../image-build/scripts/build-images.sh), and
  [Omni Dockerfile](../image-build/omni/Dockerfile).
- [Harness downloads](../pkg/hub/harness_config_handlers.go),
  [raw file handlers](../pkg/hub/harness_config_file_handlers.go), and
  [template downloads](../pkg/hub/template_handlers.go).
- [Service schema](../pkg/api/types.go),
  [startup order](../cmd/sciontool/commands/init.go), and
  [service manager](../pkg/sciontool/services/manager.go).
- [Docker argument generation](../pkg/runtime/common.go),
  [agent runtime config](../pkg/agent/run.go), and
  [searchable label validation](../pkg/labels/labels.go).
- [Worktree provisioning](../pkg/provision/provision.go) and
  [fallback handling](../pkg/runtimebroker/start_context.go).
- [Harness home layering](https://googlecloudplatform.github.io/scion/reference/harness-settings/#seeding-from-harness-configs--templates).
- [Docker container label immutability](https://docs.docker.com/engine/manage-resources/labels/#manage-labels-on-objects).
