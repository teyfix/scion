#!/usr/bin/env python3
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
"""jcode container-side provisioner."""

from __future__ import annotations

import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import scion_harness  # type: ignore[import-not-found]

assert scion_harness.INTERFACE_VERSION >= 2, (
    "jcode provision.py requires scion_harness INTERFACE_VERSION >= 2; "
    f"got {scion_harness.INTERFACE_VERSION}"
)

CODEX_AUTH_FILE = "~/.codex/auth.json"

AUTH = scion_harness.AuthSpec(
    "jcode",
    [
        scion_harness.env_method(
            "api-key",
            any_of=["OPENAI_API_KEY"],
            hint="set OPENAI_API_KEY",
        ),
        scion_harness.file_method(
            "auth-file",
            path=CODEX_AUTH_FILE,
            hint=f"provide broker-local Codex credentials at {CODEX_AUTH_FILE}",
            secret_key="CODEX_AUTH",
        ),
    ],
)


def _install_codex_auth(ctx: scion_harness.ProvisionContext) -> None:
    """Install a staged broker-local Codex auth file without changing it."""
    content = ctx.read_file_secret("CODEX_AUTH")
    if not content:
        # The host may already have mounted the declared file at its target.
        if os.path.isfile(scion_harness.expand_path(CODEX_AUTH_FILE)):
            return
        raise scion_harness.ProvisionError("CODEX_AUTH secret is empty")

    try:
        parsed = json.loads(content)
    except json.JSONDecodeError as exc:
        raise scion_harness.ProvisionError(
            f"CODEX_AUTH secret is not valid JSON: {exc}"
        ) from exc
    if not isinstance(parsed, dict):
        raise scion_harness.ProvisionError("CODEX_AUTH secret must be a JSON object")

    target = scion_harness.expand_path(CODEX_AUTH_FILE)
    os.makedirs(os.path.dirname(target), exist_ok=True)
    tmp = target + ".tmp"
    with open(tmp, "w", encoding="utf-8") as handle:
        handle.write(content)
        if not content.endswith("\n"):
            handle.write("\n")
    os.chmod(tmp, 0o600)
    os.replace(tmp, target)


def provision(ctx: scion_harness.ProvisionContext) -> None:
    resolved = ctx.select_auth(AUTH)

    env: dict[str, str] = {
        "JCODE_ALLOW_CODEX_LEGACY_AUTH": "1",
        "JCODE_NO_TELEMETRY": "1",
    }
    if resolved.method == "api-key":
        if not ctx.read_secret(resolved.env_key):
            raise scion_harness.ProvisionError(
                f"chose api-key ({resolved.env_key}) but no secret value was staged"
            )
        env["OPENAI_API_KEY"] = "${OPENAI_API_KEY}"
    elif resolved.method == "auth-file":
        _install_codex_auth(ctx)

    harness_cfg = ctx.harness_config
    instructions_file = str(harness_cfg.get("instructions_file") or "AGENTS.md")
    scion_harness.project_instructions(ctx, instructions_file)

    mcp_mapping = harness_cfg.get("mcp") or {}
    if mcp_mapping:
        scion_harness.apply_mcp_servers_simple(
            ctx.bundle_dir, mcp_mapping, ctx.workspace
        )

    extra = {"auth_file_written": True} if resolved.method == "auth-file" else None
    ctx.write_outputs(resolved, env=env, extra=extra)
    ctx.info(f"method={resolved.method}")


if __name__ == "__main__":
    scion_harness.run("jcode", provision)
