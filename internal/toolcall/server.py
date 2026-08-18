"""Tool calling sidecar — HTTP service wrapping the xyml package.

Endpoints:
  GET  /health       → {"status": "ok"}
  POST /parse        → parse tool calls from text
  POST /instructions → build XYML instruction text

Uses only stdlib; zero third-party dependencies.
"""

import http.server
import json
import os
import sys
import traceback

# Ensure xyml package is importable
_xyml_dir = os.path.join(os.path.dirname(os.path.abspath(__file__)), "xyml")
if _xyml_dir not in sys.path:
    sys.path.insert(0, os.path.dirname(_xyml_dir))  # parent of xyml/

from xyml import parse_tool_calls, build_tool_instructions  # noqa: E402

HOST = "127.0.0.1"
PORT = 17090


class ToolCallHandler(http.server.BaseHTTPRequestHandler):
    """HTTP handler for tool calling sidecar."""

    def do_GET(self):
        if self.path == "/health":
            self._json_response({"status": "ok"})
        else:
            self._json_response({"error": "not found"}, 404)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length) if length > 0 else b"{}"
        try:
            data = json.loads(body)
        except json.JSONDecodeError as e:
            self._json_response({"error": f"invalid json: {e}"}, 400)
            return

        if self.path == "/parse":
            self._handle_parse(data)
        elif self.path == "/instructions":
            self._handle_instructions(data)
        else:
            self._json_response({"error": "not found"}, 404)

    def _handle_parse(self, data):
        text = data.get("text", "")
        tools = data.get("tools")
        try:
            calls = parse_tool_calls(text, tools)
            result = []
            for c in calls:
                result.append({
                    "id": c.id,
                    "type": "function",
                    "function": {
                        "name": c.name,
                        "arguments": json.dumps(c.input) if isinstance(c.input, dict) else str(c.input),
                    },
                })
            self._json_response({"tool_calls": result})
        except Exception as e:
            tb = traceback.format_exc()
            self._json_response({"error": str(e), "traceback": tb}, 500)

    def _handle_instructions(self, data):
        tools = data.get("tools")
        if not tools:
            self._json_response({"instructions": ""})
            return
        try:
            instructions = build_tool_instructions(tools)
            self._json_response({"instructions": instructions})
        except Exception as e:
            tb = traceback.format_exc()
            self._json_response({"error": str(e), "traceback": tb}, 500)

    def _json_response(self, data, status=200):
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Access-Control-Allow-Origin", "*")
        self.end_headers()
        self.wfile.write(json.dumps(data).encode("utf-8"))

    def log_message(self, format, *args):
        # Suppress default logging (BaseHTTPRequestHandler logs every request)
        pass


def run():
    server = http.server.HTTPServer((HOST, PORT), ToolCallHandler)
    server.serve_forever()


if __name__ == "__main__":
    run()