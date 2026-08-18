"""Private utilities — escaping, JSON, text helpers, config copy.

This module intentionally depends only on the Python standard library.
"""

from __future__ import annotations

import json
import re
from collections.abc import Mapping
from typing import Any, Dict, List, Optional, Tuple

# ── Full‑width → ASCII replacements ────────────────────────────
_MARKUP_TRANS = str.maketrans({
    "\uff1c": "<",   # ＜
    "\uff1e": ">",   # ＞
    "\uff0f": "/",   # ／
    "\u2215": "/",   # ∕
    "\u2044": "/",   # ⁄
    "\uff1d": "=",   # ＝
    "\uff5c": "|",   # ｜
    "\u2502": "|",   # │
    "\u2503": "|",   # ┃
    "\u258f": "|",   # ▏
    "\u2595": "|",   # ▕
    "\u201c": '"',   # "
    "\u201d": '"',   # "
    "\u2018": "'",   # '
    "\u2019": "'",   # '
    "\ufe64": "<",   # ﹤
    "\ufe65": ">",   # ﹥
})

_ZWS_TABLE = str.maketrans({
    "\u200b": None,  # zero-width space
    "\u200c": None,  # zero-width non-joiner
    "\u200d": None,  # zero-width joiner
    "\ufeff": None,  # BOM
    "\u3000": " ",   # CJK full-width space
    "\u00a0": " ",   # NBSP
})


def escape_xml(value: Any) -> str:
    s = str(value or "")
    return (
        s.replace("&", "&amp;")
        .replace("<", "&lt;")
        .replace(">", "&gt;")
        .replace('"', "&quot;")
        .replace("'", "&apos;")
    )


def html_unescape(value: Any) -> str:
    s = str(value or "")
    return (
        s.replace("&quot;", '"')
        .replace("&apos;", "'")
        .replace("&lt;", "<")
        .replace("&gt;", ">")
        .replace("&amp;", "&")
    )


def canonicalize_markup(text: str) -> str:
    return text.translate(_MARKUP_TRANS).translate(_ZWS_TABLE)


def strip_markdown_fences(text: str) -> str:
    return re.sub(r"```[a-zA-Z0-9_-]*\s*([\s\S]*?)```", r"\1", text)


def strip_json_fence(text: str) -> str:
    m = re.fullmatch(r"```(?:json)?\s*([\s\S]*?)```", text.strip(), re.IGNORECASE)
    return m.group(1).strip() if m else text.strip()


def try_json(text: Any) -> Tuple[bool, Any]:
    """Return (ok, parsed) — never raises."""
    try:
        return True, json.loads(text)
    except (TypeError, ValueError, json.JSONDecodeError):
        return False, None


def is_mapping(value: Any) -> bool:
    return isinstance(value, Mapping)


def as_list(value: Any) -> List[Any]:
    if isinstance(value, list):
        return value
    if value is None:
        return []
    return [value]


def call_value(call: Any, key: str, default: Any = None) -> Any:
    if is_mapping(call):
        return call.get(key, default)
    return getattr(call, key, default)


def first_string(*values: Any) -> str:
    for v in values:
        if isinstance(v, str) and v.strip():
            return v.strip()
    return ""


def first_defined(*values: Any) -> Any:
    return next((v for v in values if v is not None), None)


def take_option(values: Dict[str, Any], *names: str, default: Any = None) -> Any:
    """Non‑destructive: returns a copy of values with found keys removed."""
    for name in names:
        if name in values:
            values = dict(values)  # shallow copy on first hit
            return values.pop(name), values
    return default, values


def json_dumps(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"), default=str)


def stable_stringify(value: Any) -> str:
    if isinstance(value, list):
        return "[{}]".format(",".join(stable_stringify(item) for item in value))
    if is_mapping(value):
        return "{{{}}}".format(
            ",".join(
                "{}:{}".format(json_dumps(str(k)), stable_stringify(v))
                for k, v in sorted(value, key=lambda x: str(x[0]))
            )
        )
    return json_dumps(value)


def tool_alias_key(value: Any) -> str:
    return re.sub(r"[^a-z0-9]+", "", str(value or "").strip().lower())


def clip(text: Any, maximum: int) -> str:
    v = str(text or "").strip()
    return "{}...".format(v[:maximum]) if len(v) > maximum else v


def summarize_schema(schema: Any) -> str:
    return "{}" if not schema else json_dumps(schema)


def escape_regex(value: str) -> str:
    return re.escape(value)


def rename_first_present(obj: Dict[str, Any], canonical: str, *aliases: Any) -> None:
    if obj.get(canonical) is not None:
        return
    for alias in aliases:
        if obj.get(alias) is not None:
            obj[canonical] = obj.pop(alias)
            return


def repair_loose_json(text: str) -> str:
    repaired = text.strip()
    repaired = re.sub(r'"name="\s*', '"name": "', repaired, flags=re.IGNORECASE | re.DOTALL)
    repaired = re.sub(r'"name=([^",}\s]+)"', r'"name": "\1"', repaired, flags=re.IGNORECASE | re.DOTALL)
    repaired = re.sub(
        r'"(name|input|arguments|args|parameters|tool|tool_name|function_name)"\s*=\s*',
        r'"\1": ',
        repaired,
        flags=re.IGNORECASE | re.DOTALL,
    )
    repaired = re.sub(
        r'([{,]\s*)(name|input|arguments|args|parameters|tool|tool_name|function_name)\s*:',
        r'\1"\2":',
        repaired,
        flags=re.IGNORECASE | re.DOTALL,
    )
    return repaired


def recover_json_like(text: str) -> str:
    repaired = text.strip()
    unclosed_braces = repaired.count("{") - repaired.count("}")
    unclosed_brackets = repaired.count("[") - repaired.count("]")
    return repaired + ("]" * max(unclosed_brackets, 0)) + ("}" * max(unclosed_braces, 0))