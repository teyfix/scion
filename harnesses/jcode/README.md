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

## Build the Image

```sh
docker build --build-arg BASE_IMAGE=scion-base:latest -t scion-jcode:latest -f Dockerfile .
```

The first build compiles jcode's Rust workspace and can take several minutes.
Cloud Build uses the included `cloudbuild.yaml` for amd64 and arm64 images.
