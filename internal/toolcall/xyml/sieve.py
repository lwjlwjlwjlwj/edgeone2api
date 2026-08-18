"""ToolSieve — split streamed content from textual tool-call envelopes."""

from __future__ import annotations

from typing import Any, Dict, List, Optional, Union

from ._util import is_mapping, tool_alias_key
from .config import ProtocolSpec, ToolCallConfig
from .parse import _protocol_open_tag_re, parse_tool_calls


class ToolSieve:
    """Incremental reader that separates plain content from tool-call envelopes."""

    def __init__(
        self,
        tools: Any = None,
        *,
        config: Optional[ToolCallConfig] = None,
        hold_length: int = 96,
    ) -> None:
        self.config = config or ToolCallConfig()
        self.tools = tools or []
        self.pending = ""
        self.capture = ""
        self.capturing = False
        self.hold_length = hold_length

    def process_chunk(self, chunk: Any) -> List[Dict[str, Any]]:
        if not chunk:
            return []
        self.pending += str(chunk)
        events: List[Dict[str, Any]] = []
        if self.capturing:
            self.capture += self.pending
            self.pending = ""
            consumed = self._consume_capture(force=False)
            if consumed:
                events.extend(consumed)
            return events
        start = _first_tool_marker_index(self.pending, self.config)
        if start >= 0:
            prefix = self.pending[:start]
            if prefix:
                events.append({"type": "content", "text": prefix})
            self.capture = self.pending[start:]
            self.pending = ""
            self.capturing = True
            consumed = self._consume_capture(force=False)
            if consumed:
                events.extend(consumed)
            return events
        if len(self.pending) <= self.hold_length:
            return events
        safe = self.pending[: -self.hold_length]
        self.pending = self.pending[-self.hold_length:]
        if safe:
            events.append({"type": "content", "text": safe})
        return events

    def flush(self) -> List[Dict[str, Any]]:
        events: List[Dict[str, Any]] = []
        if self.capturing and self.capture:
            consumed = self._consume_capture(force=True)
            if consumed:
                events.extend(consumed)
            elif _has_open_protocol_block(self.capture, self.config):
                pass  # captured tool structure that failed to parse — drop markup
            else:
                events.append({"type": "content", "text": self.capture})
            self.capture = ""
            self.capturing = False
        if self.pending:
            events.append({"type": "content", "text": self.pending})
            self.pending = ""
        return events

    def _consume_capture(self, force: bool) -> Optional[List[Dict[str, Any]]]:
        if (
            not force
            and _has_open_protocol_block(self.capture, self.config)
            and not _looks_structurally_closed(self.capture, self.config)
        ):
            return None
        calls = parse_tool_calls(self.capture, self.tools, config=self.config)
        if not calls:
            return None
        self.capture = ""
        self.capturing = False
        return [{"type": "tool_calls", "calls": calls}]

    processChunk = process_chunk  # backward-compat JS alias


def _first_tool_marker_index(text: str, config: ToolCallConfig) -> int:
    indexes: List[int] = []
    for protocol in config.parse_protocols:
        for tag in (protocol.tags["root"], protocol.tags["invoke"]):
            m = _protocol_open_tag_re(protocol, tag).search(text)
            if m:
                indexes.append(m.start())
    for expression in (r'^\s*\{\s*"tool_calls"', r"function\.name\s*:"):
        m = re_search(expression, text)
        if m:
            indexes.append(m.start())
    return min(indexes) if indexes else -1


def _has_open_protocol_block(text: str, config: ToolCallConfig) -> bool:
    return any(
        _protocol_open_tag_re(protocol, protocol.tags["root"]).search(text)
        or _protocol_open_tag_re(protocol, protocol.tags["invoke"]).search(text)
        for protocol in config.parse_protocols
    )


def _looks_structurally_closed(text: str, config: ToolCallConfig) -> bool:
    if re_search(r"\n\s*[\]}]\s*$", text):
        return True
    for protocol in config.parse_protocols:
        expression = r"<\s*/\s*(?:[|:]\s*)*(?:{}\s*)?(?:\s*[|:]\s*)*{}\s*>".format(
            re_escape(protocol.name), re_escape(protocol.tags["root"])
        )
        if re_search(expression, text, re.IGNORECASE):
            return True
    return False


import re  # noqa: E402

re_search = re.search
re_escape = re.escape