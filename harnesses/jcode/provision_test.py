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

from __future__ import annotations

import importlib.util
import json
import os
import tempfile
import unittest
from contextlib import contextmanager
from unittest.mock import patch

PROVISION_PATH = os.path.join(os.path.dirname(__file__), "provision.py")
SPEC = importlib.util.spec_from_file_location("jcode_provision", PROVISION_PATH)
assert SPEC is not None
provision = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(provision)

scion_harness = provision.scion_harness


@contextmanager
def temporary_home(path: str):
    old_home = os.environ.get("HOME")
    os.environ["HOME"] = path
    try:
        yield
    finally:
        if old_home is None:
            os.environ.pop("HOME", None)
        else:
            os.environ["HOME"] = old_home


def make_context(
    root: str,
    *,
    candidates: dict | None = None,
    config: dict | None = None,
    mcp_servers: dict | None = None,
    instructions: str = "",
) -> tuple[scion_harness.ProvisionContext, str, str]:
    home = os.path.join(root, "home")
    bundle = os.path.join(root, "bundle")
    inputs = os.path.join(bundle, "inputs")
    outputs = os.path.join(bundle, "outputs")
    os.makedirs(home)
    os.makedirs(inputs)
    os.makedirs(outputs)

    if candidates is not None:
        with open(os.path.join(inputs, "auth-candidates.json"), "w", encoding="utf-8") as handle:
            json.dump(candidates, handle)
    if mcp_servers is not None:
        with open(os.path.join(inputs, "mcp-servers.json"), "w", encoding="utf-8") as handle:
            json.dump({"mcp_servers": mcp_servers}, handle)
    if instructions:
        with open(os.path.join(inputs, "instructions.md"), "w", encoding="utf-8") as handle:
            handle.write(instructions)

    harness_config = config or {
        "instructions_file": "AGENTS.md",
        "system_prompt_mode": "prepend_to_instructions",
        "no_auth": {"behavior": "allow"},
        "mcp": {
            "global_config_file": ".jcode/mcp.json",
            "global_config_path": "mcpServers",
            "transport_field": "type",
            "transport_map": {"stdio": "stdio"},
        },
    }
    manifest = {
        "harness_bundle_dir": bundle,
        "agent_home": home,
        "agent_workspace": "/workspace",
        "harness_config": harness_config,
    }
    return scion_harness.ProvisionContext("jcode", manifest), home, bundle


class JcodeProvisionTest(unittest.TestCase):
    def test_bridges_http_preserving_headers_and_existing_servers(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            server = {
                "transport": "streamable-http",
                "url": "http://knowledge:8000/mcp?project=pilot",
                "headers": {"Authorization": "Bearer ${MCP_TOKEN}"},
                "scope": "project",
            }
            ctx, home, bundle = make_context(root, candidates={}, mcp_servers={"basic-memory": server})
            target = os.path.join(home, ".jcode", "mcp.json")
            os.makedirs(os.path.dirname(target))
            with open(target, "w", encoding="utf-8") as handle:
                json.dump({"mcpServers": {"existing": {"command": "files"}}, "other": True}, handle)
            with temporary_home(home), patch.object(provision.shutil, "which", return_value="/usr/bin/bun"), patch.object(provision, "MCP_BRIDGE", PROVISION_PATH):
                provision.provision(ctx)
            with open(target, encoding="utf-8") as handle:
                config = json.load(handle)
            self.assertTrue(config["other"])
            self.assertEqual(config["mcpServers"]["existing"], {"command": "files"})
            self.assertEqual(config["mcpServers"]["basic-memory"], {
                "type": "stdio", "command": "bun",
                "args": [PROVISION_PATH, server["url"], "--transport", "http-only", "--silent", "--allow-http", "--header", "Authorization: Bearer ${MCP_TOKEN}"],
            })
            self.assertEqual(os.stat(target).st_mode & 0o777, 0o600)
            with open(os.path.join(bundle, "inputs", "mcp-servers.json"), encoding="utf-8") as handle:
                self.assertEqual(json.load(handle)["mcp_servers"]["basic-memory"], server)

    def test_https_does_not_allow_plaintext(self) -> None:
        with patch.object(provision.shutil, "which", return_value="bun"), patch.object(provision, "MCP_BRIDGE", PROVISION_PATH):
            entry = provision._mcp_entry("memory", {"transport": "streamable-http", "url": "https://knowledge.example/mcp"})
        self.assertNotIn("--allow-http", entry["args"])

    def test_rejects_unsupported_transport_and_invalid_endpoint(self) -> None:
        for spec in [
            {"transport": "sse", "url": "http://knowledge/sse"},
            *({"transport": "streamable-http", "url": url} for url in [
                "file:///etc/passwd", "http://", "http://user:password@knowledge/mcp", "http://knowledge:99999/mcp", "http://knowledge/mcp#fragment",
            ]),
        ]:
            with self.subTest(spec=spec), self.assertRaises(scion_harness.ProvisionError):
                provision._mcp_entry("memory", spec)

    def test_missing_bridge_and_malformed_headers_fail(self) -> None:
        server = {"transport": "streamable-http", "url": "http://knowledge/mcp"}
        with patch.object(provision.shutil, "which", return_value=None), self.assertRaises(scion_harness.ProvisionError):
            provision._mcp_entry("memory", server)
        with patch.object(provision.shutil, "which", return_value="bun"), patch.object(provision, "MCP_BRIDGE", "/nonexistent/scion-bridge"), self.assertRaises(scion_harness.ProvisionError):
            provision._mcp_entry("memory", server)
        for headers in [{"bad:header": "value"}, {"Authorization": "Bearer\nsecret"}, {"X-Test": 123}, ["value"]]:
            with self.subTest(headers=headers), patch.object(provision.shutil, "which", return_value="bun"), patch.object(provision, "MCP_BRIDGE", PROVISION_PATH), self.assertRaises(scion_harness.ProvisionError):
                provision._mcp_entry("memory", {**server, "headers": headers})

    def test_malformed_existing_config_fails_without_overwriting(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            ctx, home, _ = make_context(root, mcp_servers={"files": {"transport": "stdio", "command": "files"}})
            target = os.path.join(home, ".jcode", "mcp.json")
            os.makedirs(os.path.dirname(target))
            with open(target, "w", encoding="utf-8") as handle:
                handle.write("not json")
            with temporary_home(home), self.assertRaises(scion_harness.ProvisionError):
                provision._apply_mcp_servers(ctx, ctx.harness_config["mcp"])
            with open(target, encoding="utf-8") as handle:
                self.assertEqual(handle.read(), "not json")

    def test_installs_broker_local_codex_auth_file(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            ctx, home, bundle = make_context(
                root,
                candidates={
                    "file_secret_files": {
                        "CODEX_AUTH": os.path.join(root, "bundle", "secrets", "CODEX_AUTH")
                    }
                },
            )
            os.makedirs(os.path.join(bundle, "secrets"))
            secret = '{"auth_mode":"oauth","tokens":{"access_token":"test"}}'
            with open(os.path.join(bundle, "secrets", "CODEX_AUTH"), "w", encoding="utf-8") as handle:
                handle.write(secret)

            with temporary_home(home):
                provision.provision(ctx)

            target = os.path.join(home, ".codex", "auth.json")
            with open(target, "r", encoding="utf-8") as handle:
                self.assertEqual(json.load(handle)["auth_mode"], "oauth")
            self.assertEqual(os.stat(target).st_mode & 0o777, 0o600)

    def test_rejects_invalid_codex_auth_json(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            ctx, home, bundle = make_context(
                root,
                candidates={
                    "file_secret_files": {
                        "CODEX_AUTH": os.path.join(root, "bundle", "secrets", "CODEX_AUTH")
                    }
                },
            )
            os.makedirs(os.path.join(bundle, "secrets"))
            with open(os.path.join(bundle, "secrets", "CODEX_AUTH"), "w", encoding="utf-8") as handle:
                handle.write("not json")

            with temporary_home(home), self.assertRaises(scion_harness.ProvisionError):
                provision.provision(ctx)

    def test_projects_instructions_and_stdio_mcp(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            ctx, home, _ = make_context(
                root,
                candidates={},
                instructions="Work one item at a time.",
                mcp_servers={
                    "files": {
                        "transport": "stdio",
                        "command": "mcp-files",
                        "args": ["/workspace"],
                        "scope": "project",
                    }
                },
            )

            with temporary_home(home):
                provision.provision(ctx)

            with open(os.path.join(home, "AGENTS.md"), "r", encoding="utf-8") as handle:
                self.assertIn("Work one item at a time.", handle.read())
            with open(os.path.join(home, ".jcode", "mcp.json"), "r", encoding="utf-8") as handle:
                mcp = json.load(handle)
            self.assertEqual(mcp["mcpServers"]["files"]["type"], "stdio")
            self.assertEqual(mcp["mcpServers"]["files"]["command"], "mcp-files")

    def test_api_key_output_uses_secret_reference(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            ctx, home, bundle = make_context(
                root,
                candidates={
                    "env_vars": ["OPENAI_API_KEY"],
                    "env_secret_files": {
                        "OPENAI_API_KEY": os.path.join(root, "bundle", "secrets", "OPENAI_API_KEY")
                    },
                },
            )
            os.makedirs(os.path.join(bundle, "secrets"))
            with open(os.path.join(bundle, "secrets", "OPENAI_API_KEY"), "w", encoding="utf-8") as handle:
                handle.write("test-key")

            with temporary_home(home):
                provision.provision(ctx)

            with open(os.path.join(bundle, "outputs", "env.json"), "r", encoding="utf-8") as handle:
                env = json.load(handle)
            self.assertEqual(env["OPENAI_API_KEY"], "${OPENAI_API_KEY}")
            self.assertEqual(env["JCODE_NO_TELEMETRY"], "1")
            self.assertNotIn("test-key", json.dumps(env))


if __name__ == "__main__":
    unittest.main()
