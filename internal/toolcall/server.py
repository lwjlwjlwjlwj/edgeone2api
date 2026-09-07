"""Tool calling sidecar — HTTP service wrapping the xyml engine + fc policy layer.

The gateway delegates ALL tool-call logic here (ported from toolforge
refactor/xyml-package): instruction building, history rendering, output
parsing (markup/XML/JSON/text-KV with CDATA-aware recovery) and truncation /
parse-error recovery.  The Go process stays a thin transport.

Endpoints:
  GET  /health          → {"status": "ok"}
  POST /instructions    → build XYML instruction block (+ optional tool profile)
  POST /render_history  → flatten OpenAI tool history into plain chat messages
  POST /parse           → parse model output into OpenAI tool_calls
  POST /recover         → parse + truncation / parse-error recovery hint
  POST /retry_message   → build the corrective user message for a retry turn

Uses only stdlib; zero third-party dependencies.
"""

import http.server
import json
import os
import sys
import traceback

# Ensure xyml / fc / models packages are importable.
_xyml_dir = os.path.join(os.path.dirname(os.path.abspath(__file__)), "xyml")
if _xyml_dir not in sys.path:
    sys.path.insert(0, os.path.dirname(_xyml_dir))  # parent of xyml/ = internal/toolcall/

from xyml import (  # noqa: E402
    ProtocolSpec,
    ToolCallConfig,
    build_tool_instructions,
    normalize_tools,
)
from models.canonical import (  # noqa: E402
    openai_messages_to_canonical,
    openai_tools_to_defs,
    tool_defs_to_openai,
)
from fc.inject import render_history_messages  # noqa: E402
from fc.parse import parse_text_to_calls, to_openai_tool_calls  # noqa: E402
from fc.recovery import (  # noqa: E402
    build_retry_user_message,
    parse_with_recovery_hint,
)
from fc.profiles import detect_tool_profile, profile_instruction_block  # noqa: E402

HOST = "127.0.0.1"
PORT = 17090

# Gateway parse policy: keep unknown / incomplete calls so the Go translation
# layer (native-name aliasing, declared-set validation) decides what to emit.


def _gateway_config(protocol: str = "XYML") -> ToolCallConfig:
    emit = (protocol or "XYML").strip() or "XYML"
    return ToolCallConfig(
        emit_protocol=emit,
        parse_protocols=[
            ProtocolSpec(emit),
            ProtocolSpec("QNML", parse_only=True),
            ProtocolSpec("XYML", parse_only=True),
        ],
        unknown_tool="keep",
        missing_required="keep",
        prompt_style="standard",
    )


def _tools_list(raw_tools: object):
    if not raw_tools:
        return []
    return openai_tools_to_defs(raw_tools)


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

        routes = {
            "/instructions": self._handle_instructions,
            "/render_history": self._handle_render_history,
            "/parse": self._handle_parse,
            "/recover": self._handle_recover,
            "/retry_message": self._handle_retry_message,
        }
        handler = routes.get(self.path)
        if handler is None:
            self._json_response({"error": "not found"}, 404)
            return
        try:
            handler(data)
        except Exception as e:
            tb = traceback.format_exc()
            self._json_response({"error": str(e), "traceback": tb}, 500)

    def _handle_instructions(self, data):
        tools = _tools_list(data.get("tools"))
        protocol = data.get("protocol") or "XYML"
        if not tools:
            self._json_response({"instructions": ""})
            return
        cfg = _gateway_config(protocol)
        sdk_tools = normalize_tools(tool_defs_to_openai(tools))
        instructions = build_tool_instructions(sdk_tools, config=cfg)
        profile = detect_tool_profile(tools)
        block = profile_instruction_block(profile)
        self._json_response({
            "instructions": instructions,
            "profile": {
                "id": profile.id,
                "display_name": profile.display_name,
                "rules": profile.rules,
                "block": block,
            },
        })

    def _handle_render_history(self, data):
        protocol = data.get("protocol") or "XYML"
        messages = openai_messages_to_canonical(data.get("messages") or [])
        if not messages:
            self._json_response({"items": []})
            return
        items = render_history_messages(messages, protocol=protocol)
        self._json_response({"items": items})

    def _handle_parse(self, data):
        text = str(data.get("text") or "")
        tools = _tools_list(data.get("tools"))
        protocol = data.get("protocol") or "XYML"
        strip_think = bool(data.get("strip_think", True))
        calls = parse_text_to_calls(
            text,
            tools,
            protocol=protocol,
            strip_think=strip_think,
            config=_gateway_config(protocol),
        )
        self._json_response({"tool_calls": to_openai_tool_calls(calls)})

    def _handle_recover(self, data):
        text = str(data.get("text") or "")
        tools = _tools_list(data.get("tools"))
        protocol = data.get("protocol") or "XYML"
        calls, reason = parse_with_recovery_hint(
            text,
            tools,
            protocol=protocol,
            strip_think=True,
            config=_gateway_config(protocol),
        )
        self._json_response({
            "tool_calls": to_openai_tool_calls(calls),
            "reason": reason,
        })

    def _handle_retry_message(self, data):
        original_output = str(data.get("original_output") or "")
        reason = str(data.get("reason") or "parse_failed")
        message = build_retry_user_message(original_output=original_output, reason=reason)
        self._json_response({"message": message})

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
