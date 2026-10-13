# jcode Harness Bundle

Scion harness configuration for [jcode](https://github.com/1jehuang/jcode),
an open-source coding agent with native ChatGPT/Codex subscription support.

The image pins jcode v0.58.0 and builds it from source with
`--no-default-features` to keep upstream's optional default feature set out of
the pilot image. Actual memory savings should be measured under representative
concurrent workloads.

jcode's anonymous usage telemetry is disabled in both the image and launch
environment.

## Install

From a repository checkout:

```sh
scion harness-config install harnesses/jcode
```

Or directly from GitHub:

```sh
scion harness-config install github.com/GoogleCloudPlatform/scion/tree/main/harnesses/jcode
```

## Auth Modes

| Mode | Env / File | Notes |
|------|------------|-------|
| `auth-file` (default) | user's `~/.codex/auth.json` | Auto-discovered in local mode; a broker deployment can project its own user's file into the agent home |
| `api-key` | `OPENAI_API_KEY` | Uses the OpenAI API-key route |

With no Scion-managed credentials, the harness starts normally and lets jcode
use credentials supplied by the broker/container lifecycle. This supports a
separate account per broker without requiring the Hub to share credentials.

## Execution Model

Scion launches `jcode run <task>`. This is jcode's supported autonomous,
streaming single-task mode. Its interactive TUI does not currently expose an
initial-prompt argument, so automatic TUI startup and session-ID resume are
reported as partial capabilities rather than emulated with terminal input.

jcode reads Scion instructions from `~/AGENTS.md`, repository instructions
from each project's `AGENTS.md`, skills from `~/.jcode/skills`, and stdio MCP
servers from `~/.jcode/mcp.json`.

## Shared HTTP MCP

Native remote MCP is still tracked in [jcode issue #483](https://github.com/1jehuang/jcode/issues/483).
This bundle supports Streamable HTTP through a pinned
[mcp-remote](https://github.com/geelen/mcp-remote) v0.1.38 stdio bridge running
under Bun. Configure the usual Scion server entry, for example:

```yaml
mcp_servers:
  basic-memory:
    transport: streamable-http
    url: http://knowledge:8000/mcp
    # Optional; inherited from this broker's agent environment at runtime:
    # headers:
    #   Authorization: 'Bearer ${MCP_TOKEN}'
```

The provisioner preserves the server name and translates this to a Bun stdio
command in `~/.jcode/mcp.json`. Project scope is demoted to global, as with
native stdio entries. Endpoint query parameters and headers are preserved;
invalid endpoints, legacy SSE, or a missing bridge fail provisioning. HTTP-only
transport prevents a silent fallback to legacy SSE. HTTPS is preferred; use
plain HTTP only on a trusted network. Header values stay out of bridge logs
(`--silent`) but literal credentials are present in its private config and
process arguments; prefer `${ENV_VAR}` references. Initial connection failures terminate
the bridge with a nonzero status rather than selecting another transport.

There is one bridge process per configured HTTP server, not a per-agent Python
MCP service. The shared service, its network/DNS, and broker-local credentials
remain deployment responsibilities. Bun still adds per-agent RAM usage; measure
it with your workload before increasing concurrency. Python provisioning exits
before the agent starts. No package downloads occur at agent startup.
The localhost smoke fixture measured roughly 62 MiB RSS for the bridge under
Bun v1.4.2; this is not a concurrent-agent or real-service memory benchmark.

Custom images copying the jcode binary must also copy
`/usr/local/lib/scion/jcode-mcp-remote.js` (and its license) from the Scion jcode
image and provide Bun on `PATH`. Merely copying `/usr/local/bin/jcode` does not
enable HTTP MCP. Existing native stdio configurations do not require the bridge.

## Build the Image

```sh
docker build --build-arg BASE_IMAGE=scion-base:latest -t scion-jcode:latest -f Dockerfile .
```

The first build compiles jcode's Rust workspace and can take several minutes.
Cloud Build uses the included `cloudbuild.yaml` for amd64 and arm64 images.
The bridge has an independent build stage, so it can build alongside Rust.
Its direct dependency and dependency tree are pinned in `mcp-remote/package.json`
and `mcp-remote/bun.lock`.

To test the actual bridge without compiling Rust, bundle it in a scratch directory
using those two files and run:

```sh
SCION_JCODE_MCP_BRIDGE=/path/to/jcode-mcp-remote.js python3 mcp-remote/smoke_test.py
```

The smoke test uses a local HTTP fixture; it does not contact a model provider
or require credentials.
