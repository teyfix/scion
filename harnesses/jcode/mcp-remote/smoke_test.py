#!/usr/bin/env python3
"""Exercise the bundled Bun bridge against a credential-free HTTP MCP fixture."""

from __future__ import annotations

import json
import os
import selectors
import shutil
import signal
import subprocess
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Fixture(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def reply(self, status, payload=None):
        body = json.dumps(payload).encode() if payload is not None else b""
        use_sse = status == 200 and self.server.sse
        if use_sse:
            body = b"event: message\ndata: " + body + b"\n\n"
        self.send_response(status)
        self.send_header("Content-Type", "text/event-stream" if use_sse else "application/json")
        if status == 200:
            self.send_header("Mcp-Session-Id", "fixture-session")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self.server.requests.append(("GET", self.path, {key.lower(): value for key, value in self.headers.items()}, None))
        self.reply(405 if self.path.startswith("/mcp") else 404)

    def do_POST(self):
        request = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        self.server.requests.append(("POST", self.path, {key.lower(): value for key, value in self.headers.items()}, request))
        if self.server.fail:
            self.reply(404)
            return
        method = request.get("method")
        if "id" not in request:
            self.reply(202)
            return
        if method == "initialize":
            result = {
                "protocolVersion": request["params"]["protocolVersion"],
                "capabilities": {"tools": {}},
                "serverInfo": {"name": "shared-knowledge-fixture", "version": "1"},
            }
        elif method == "tools/list":
            result = {"tools": [{"name": "read_note", "description": "Read shared note", "inputSchema": {"type": "object", "properties": {}}}]}
        elif method == "tools/call":
            result = {"content": [{"type": "text", "text": "shared knowledge works"}]}
        else:
            self.reply(400)
            return
        self.reply(200, {"jsonrpc": "2.0", "id": request["id"], "result": result})


@unittest.skipUnless(os.environ.get("SCION_JCODE_MCP_BRIDGE") and shutil.which("bun"), "set SCION_JCODE_MCP_BRIDGE to the bundled JS and install Bun")
class BridgeSmokeTest(unittest.TestCase):
    def setUp(self):
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Fixture)
        self.server.requests = []
        self.server.fail = False
        self.server.sse = False
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.home = tempfile.TemporaryDirectory()
        self.process = None

    def tearDown(self):
        if self.process:
            if self.process.poll() is None:
                self.process.kill()
            self.process.wait(timeout=5)
            for stream in (self.process.stdin, self.process.stdout, self.process.stderr):
                stream.close()
        self.server.shutdown()
        self.server.server_close()
        self.home.cleanup()

    def launch(self):
        endpoint = f"http://127.0.0.1:{self.server.server_port}/mcp?project=pilot"
        self.process = subprocess.Popen([
            "bun", os.environ["SCION_JCODE_MCP_BRIDGE"], endpoint,
            "--transport", "http-only", "--silent", "--allow-http",
            "--header", "Authorization: Bearer ${MCP_FIXTURE_TOKEN}",
        ], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            env={**os.environ, "HOME": self.home.name, "MCP_FIXTURE_TOKEN": "fixture-private-token"}, text=True)

    def send(self, payload):
        self.process.stdin.write(json.dumps({"jsonrpc": "2.0", **payload}) + "\n")
        self.process.stdin.flush()

    def response(self):
        with selectors.DefaultSelector() as selector:
            selector.register(self.process.stdout, selectors.EVENT_READ)
            self.assertTrue(selector.select(timeout=10), "bridge response timed out")
        line = self.process.stdout.readline()
        self.assertTrue(line, f"bridge exited with {self.process.poll()}")
        return json.loads(line)

    def initialize(self):
        self.launch()
        self.send({"id": 1, "method": "initialize", "params": {"protocolVersion": "2024-11-05", "capabilities": {}, "clientInfo": {"name": "jcode-stdio-fixture", "version": "1"}}})
        self.assertEqual(self.response()["result"]["serverInfo"]["name"], "shared-knowledge-fixture")
        self.send({"method": "notifications/initialized"})

    def test_initialize_list_read_and_eof(self):
        self.initialize()
        self.send({"id": 2, "method": "tools/list", "params": {}})
        self.assertEqual(self.response()["result"]["tools"][0]["name"], "read_note")
        self.send({"id": 3, "method": "tools/call", "params": {"name": "read_note", "arguments": {}}})
        self.assertEqual(self.response()["result"]["content"][0]["text"], "shared knowledge works")
        with open(f"/proc/{self.process.pid}/status", encoding="utf-8") as status:
            print("Bun bridge " + next(line.strip() for line in status if line.startswith("VmRSS:")))
        self.process.stdin.close()
        self.assertEqual(self.process.wait(timeout=5), 0)
        self.assertEqual(self.process.stdout.read(), "", "stdout must contain only MCP responses")
        self.assertEqual(self.process.stderr.read(), "", "headers/tool contents must not enter logs")
        calls = [item for item in self.server.requests if item[0] == "POST"]
        self.assertTrue(any(item[3].get("method") == "tools/call" for item in calls))
        self.assertTrue(all(item[1] == "/mcp?project=pilot" for item in calls))
        self.assertTrue(all(item[2].get("authorization") == "Bearer fixture-private-token" for item in calls))
        tool_calls = [item for item in calls if item[3].get("method", "").startswith("tools/")]
        self.assertTrue(all(item[2].get("mcp-session-id") == "fixture-session" for item in tool_calls))

    def test_streamable_http_sse_encoded_response(self):
        # SSE framing on a POST response is valid Streamable HTTP, not legacy SSE.
        self.server.sse = True
        self.initialize()
        self.send({"id": 2, "method": "tools/list", "params": {}})
        self.assertEqual(self.response()["result"]["tools"][0]["name"], "read_note")

    def test_sigterm_exits(self):
        self.initialize()
        self.process.send_signal(signal.SIGTERM)
        self.process.wait(timeout=5)
        self.assertEqual(self.process.stderr.read(), "")

    def test_failed_http_does_not_fall_back_to_sse(self):
        self.server.fail = True
        self.launch()
        self.assertNotEqual(self.process.wait(timeout=10), 0)
        self.assertEqual(self.process.stdout.read(), "")
        self.assertEqual(self.process.stderr.read(), "")
        # OAuth discovery probes GET with both content types. A legacy SSE
        # fallback would instead GET with Accept: text/event-stream only.
        probes = [headers.get("accept") for method, path, headers, _request in self.server.requests if method == "GET" and path.startswith("/mcp")]
        self.assertEqual(probes, ["application/json, text/event-stream"])


if __name__ == "__main__":
    unittest.main()
