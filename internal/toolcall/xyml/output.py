"""Output formatters — convert parsed ToolCall to OpenAI/Anthropic/Responses format."""

from __future__ import annotations

from typing import Any, Dict, List

from ._util import call_value, json_dumps, as_list
from .config import ToolCall, random_id


def openai_tool_calls(calls: Any) -> List[Dict[str, Any]]:
    return [
        {
            "id": call_value(call, "id"),
            "type": "function",
            "function": {
                "name": call_value(call, "name"),
                "arguments": _arguments_string(call_value(call, "input", {})),
            },
        }
        for call in as_list(calls)
    ]


def responses_tool_items(calls: Any) -> List[Dict[str, Any]]:
    return [
        {
            "id": "fc_{}".format(random_id(12)),
            "type": "function_call",
            "status": "completed",
            "call_id": call_value(call, "id"),
            "name": call_value(call, "name"),
            "arguments": _arguments_string(call_value(call, "input", {})),
        }
        for call in as_list(calls)
    ]


def anthropic_tool_use_blocks(calls: Any) -> List[Dict[str, Any]]:
    return [
        {
            "type": "tool_use",
            "id": call_value(call, "id"),
            "name": call_value(call, "name"),
            "input": call_value(call, "input", {}),
        }
        for call in as_list(calls)
    ]


def _arguments_string(value: Any) -> str:
    return value if isinstance(value, str) else json_dumps({} if value is None else value)