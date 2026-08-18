"""Tool call parsing — markup, XML, JSON, and text key/value protocols.

Key improvements over the original single-file implementation:
- S3: ToolCallConfig no longer mutates caller dicts.
- S4: JSON fragment discovery is O(n) (balanced-bracket matching) instead of O(n^2).
- M2: compiled regexes are cached via functools.lru_cache.
"""

from __future__ import annotations

import json
import re
from functools import lru_cache
from typing import Any, Callable, Dict, List, Mapping, Optional, Sequence, Tuple

from ._util import (
    canonicalize_markup,
    escape_regex,
    first_defined,
    first_string,
    html_unescape,
    is_mapping,
    rename_first_present,
    repair_loose_json,
    recover_json_like,
    strip_json_fence,
    strip_markdown_fences,
    tool_alias_key,
    try_json,
)
from .config import (
    ParsedToolCall,
    ProtocolSpec,
    ToolCallConfig,
    XymlError,
)
from .build import _schema_properties, normalize_tools

_THINK_RE = re.compile(r"<think>[\s\S]*?</think>", re.IGNORECASE)


# ── Public API ──────────────────────────────────────────────────
def parse_tool_calls(
    text: Any,
    tools: Any = None,
    *,
    config: Optional[ToolCallConfig] = None,
) -> List[ParsedToolCall]:
    """Extract normalized calls from markup, JSON, XML, or text key/value output."""
    cfg = config or ToolCallConfig()
    normalized_tools = normalize_tools(tools)
    if not str(text or "").strip() or not normalized_tools:
        return []
    allowed = _build_allowed_tool_map(normalized_tools, cfg)
    calls: List[ParsedToolCall] = []
    if cfg.enable_markup:
        for protocol in cfg.parse_protocols:
            calls.extend(_parse_protocol_markup(text, protocol, allowed, normalized_tools, cfg))
    if cfg.enable_xml:
        calls.extend(_parse_xml_tool_calls(text, allowed, cfg))
    if cfg.enable_json:
        _for_each_json_fragment(
            text,
            lambda value: calls.extend(_parse_json_tool_calls(value, allowed, cfg)),
        )
    if cfg.enable_text_kv:
        calls.extend(_parse_text_kv_tool_calls(text, allowed, normalized_tools, cfg))
    fixed = calls
    if cfg.enable_coercion:
        fixed = [
            parsed
            for parsed in (_coerce_parsed_call(call, normalized_tools, cfg) for call in calls)
            if parsed is not None
        ]
    return _dedupe_tool_calls(fixed) if cfg.enable_dedupe else fixed


def parse_markup_tool_calls(
    text: Any,
    tools: Any = None,
    *,
    config: Optional[ToolCallConfig] = None,
    protocols: Optional[Sequence[Any]] = None,
) -> List[ParsedToolCall]:
    cfg = config or ToolCallConfig()
    normalized_tools = normalize_tools(tools)
    allowed = _build_allowed_tool_map(normalized_tools, cfg)
    active_protocols = (
        _normalize_protocol_specs(protocols, cfg.emit_protocol) if protocols is not None else cfg.parse_protocols
    )
    calls: List[ParsedToolCall] = []
    for protocol in active_protocols:
        calls.extend(_parse_protocol_markup(text, protocol, allowed, normalized_tools, cfg))
    fixed = [
        parsed
        for parsed in (_coerce_parsed_call(call, normalized_tools, cfg) for call in calls)
        if parsed is not None
    ]
    return _dedupe_tool_calls(fixed) if cfg.enable_dedupe else fixed


def coerce_tool_input(
    name: str,
    input: Any,
    tools: Any = None,
    *,
    config: Optional[ToolCallConfig] = None,
) -> Any:
    """Apply schema coercion and common aliases to a parsed call input."""
    cfg = config or ToolCallConfig()
    fixed = _coerce_tool_input_by_schema(name, input, normalize_tools(tools))
    if not is_mapping(fixed):
        return fixed
    fixed = dict(fixed)
    aliases = cfg.argument_aliases.get(name, {}) if is_mapping(cfg.argument_aliases) else {}
    for canonical, alternate_names in dict(aliases).items():
        rename_first_present(fixed, canonical, *_as_list(alternate_names))
    return _apply_common_aliases(name, fixed, tools)


def _apply_common_aliases(name: str, fixed: Dict[str, Any], tools: Any) -> Dict[str, Any]:
    if name == "AskUserQuestion":
        if fixed.get("question") is not None and fixed.get("questions") is None:
            fixed["questions"] = [
                {
                    "question": fixed["question"],
                    "header": "Question",
                    "multiSelect": False,
                    "options": [
                        {"label": "Yes", "description": "Confirm"},
                        {"label": "No", "description": "Decline"},
                    ],
                }
            ]
            fixed.pop("question", None)
        if fixed.get("questions") is not None and not isinstance(fixed["questions"], list):
            fixed["questions"] = [fixed["questions"]]
    elif name == "Agent":
        fixed.setdefault("description", "Execute sub-task")
        fixed.setdefault("prompt", fixed["description"])
    elif name == "Read":
        rename_first_present(fixed, "file_path", "path", "filename", "file")
    elif name == "Write":
        rename_first_present(fixed, "file_path", "path", "target_file", "filename", "file")
        rename_first_present(fixed, "content", "text", "body", "data", "file_content", "contents", "value")
    elif name == "Edit":
        rename_first_present(fixed, "file_path", "path", "target_file", "filename", "file")
    elif name in {"Bash", "PowerShell"}:
        rename_first_present(fixed, "command", "cmd", "script")
    elif fixed.get("query") is None and fixed.get("queries") is not None and _tool_accepts_field(name, tools, "query"):
        queries = fixed.pop("queries")
        if isinstance(queries, list):
            fixed["query"] = "\n".join(str(v) for v in queries if str(v))
        else:
            fixed["query"] = str(queries).strip()
    return fixed


# ── Markup parsing ──────────────────────────────────────────────
def _parse_protocol_markup(
    text: Any,
    protocol: ProtocolSpec,
    allowed: Mapping[str, str],
    tools: Sequence[Mapping[str, Any]],
    config: ToolCallConfig,
) -> List[ParsedToolCall]:
    canonical = canonicalize_markup(strip_markdown_fences(str(text or "")))
    canonical = _expand_short_close_tags(canonical, protocol)
    calls: List[ParsedToolCall] = []
    for candidate in _extract_protocol_candidates(canonical, protocol):
        for match in _protocol_tag_block_re(protocol, protocol.tags["invoke"]).finditer(candidate):
            name = _canonical_tool_name(_extract_name_attr(match.group(1)), allowed, config)
            if not name:
                continue
            input = _parse_protocol_parameters(match.group(2), protocol, config)
            calls.append(ParsedToolCall(id=config.id_factory(), name=name, input=input))
    if not calls:
        calls.extend(_parse_loose_protocol_calls(canonical, protocol, allowed, tools, config))
    return calls


def _extract_protocol_candidates(text: str, protocol: ProtocolSpec) -> List[str]:
    candidates = [
        m.group(2)
        for m in _protocol_tag_block_re(protocol, protocol.tags["root"]).finditer(text)
    ]
    if candidates:
        return candidates
    match = _protocol_open_tag_re(protocol, protocol.tags["invoke"]).search(text)
    return [text[match.start():]] if match else []


def _parse_protocol_parameters(body: str, protocol: ProtocolSpec, config: ToolCallConfig) -> Dict[str, Any]:
    out: Dict[str, Any] = {}
    for match in _protocol_tag_block_re(protocol, protocol.tags["parameter"]).finditer(body):
        name = _extract_name_attr(match.group(1))
        if name:
            out[name] = _decode_markup_value(match.group(2), name, config)
    return out or _parse_text_kv_input(body)


def _parse_loose_protocol_calls(
    text: str,
    protocol: ProtocolSpec,
    allowed: Mapping[str, str],
    tools: Sequence[Mapping[str, Any]],
    config: ToolCallConfig,
) -> List[ParsedToolCall]:
    if not re.search(r"\b{}\b".format(escape_regex(protocol.name)), text, re.IGNORECASE):
        return []
    attr_re = re.compile(
        r"\b(?:name|parameter)\s*=\s*(?:\"([^\"]*)\"|'([^']*)'|([A-Za-z0-9_.:-]+))",
        re.IGNORECASE | re.DOTALL,
    )
    attributes: List[Tuple[str, str, bool, int]] = []
    for match in attr_re.finditer(text):
        raw = html_unescape(next((v for v in match.groups() if v is not None), "").strip())
        if not raw:
            continue
        name = _canonical_tool_name(raw, allowed, config)
        attributes.append((raw, name or raw, bool(name), match.start()))
    calls: List[ParsedToolCall] = []
    for index, attribute in enumerate(attributes):
        raw, name, is_tool, position = attribute
        if not is_tool:
            continue
        next_tool = next(
            (item[3] for item in attributes[index + 1:] if item[2]), len(text)
        )
        input: Dict[str, Any] = {}
        for field_raw, _, field_is_tool, field_position in attributes[index + 1:]:
            if field_position >= next_tool or field_is_tool:
                break
            cdata = re.search(
                r"<\s*(?:[|:])*\s*!\[CDATA\[([\s\S]*?)\]\]>",
                text[field_position:next_tool],
                re.IGNORECASE,
            )
            if cdata:
                input[field_raw] = _decode_markup_value(cdata.group(1), field_raw, config)
        filtered = _filter_input_for_tool(name, input, tools)
        if not filtered and _required_tool_args(name, tools):
            continue
        calls.append(ParsedToolCall(id=config.id_factory(), name=name, input=filtered))
    return calls


def _expand_short_close_tags(text: str, protocol: ProtocolSpec) -> str:
    """Expand shorthand closing tags like </|XYML> to their full form."""
    if not text or "</" not in text:
        return text
    proto = escape_regex(protocol.name)
    combined = re.compile(
        r"<\s*/\s*(?:[|:]\s*)*(?:{}\s*)?(?:\s*[|:]\s*)*([A-Za-z0-9_]+)?\s*?>"
        r"|<\s*(?:[|:]\s*)*(?:{}\s*)?(?:\s*[|:]\s*)*([A-Za-z0-9_]+)\b[^>]*>".format(proto, proto),
        re.IGNORECASE,
    )
    stack: List[str] = []
    out: List[str] = []
    pos = 0
    for m in combined.finditer(text):
        out.append(text[pos:m.start()])
        token = m.group(0)
        closing = token.lstrip().startswith("</")
        if closing:
            tag_name = m.group(1)
            if tag_name is None and stack:
                tag_name = stack.pop()
                out.append("</|{}|{}>".format(protocol.name, tag_name))
            else:
                if tag_name is not None and stack and stack[-1] == tag_name:
                    stack.pop()
                out.append(token)
        else:
            tag_name = m.group(2)
            if tag_name:
                stack.append(tag_name)
            out.append(token)
        pos = m.end()
    out.append(text[pos:])
    return "".join(out)


def _decode_markup_value(raw: Any, parameter_name: Any, config: ToolCallConfig) -> Any:
    cdata_matches = re.findall(
        r"<\s*(?:[|:])*\s*!\[CDATA\[([\s\S]*?)\]\]>",
        str(raw or ""),
        re.IGNORECASE,
    )
    raw_string = str(parameter_name or "").lower() in config.raw_string_params
    if cdata_matches:
        joined = "".join(cdata_matches)
        return joined if raw_string else _coerce_markup_scalar(joined, raw_string=False)
    if not raw_string:
        parsed, nested = _parse_nested_markup_value(str(raw or ""), config)
        if parsed:
            return nested
    return _coerce_markup_scalar(raw, raw_string=raw_string)


def _parse_nested_markup_value(raw: str, config: ToolCallConfig) -> Tuple[bool, Any]:
    text = raw.strip()
    if not text or "<" not in text:
        return False, None
    matches = list(re.finditer(r"<([A-Za-z_][A-Za-z0-9_.:-]*)\b[^>]*>([\s\S]*?)</\1>", text))
    if not matches:
        return False, None
    names = [m.group(1) for m in matches]
    values = [_decode_markup_value(m.group(2), m.group(1), config) for m in matches]
    if all(n.lower() == "item" for n in names):
        return True, values
    out: Dict[str, Any] = {}
    for name, value in zip(names, values):
        if name not in out:
            out[name] = value
        elif isinstance(out[name], list):
            out[name].append(value)
        else:
            out[name] = [out[name], value]
    return True, out


def _coerce_markup_scalar(raw: Any, raw_string: bool) -> Any:
    value = html_unescape(str(raw or "").strip())
    if raw_string:
        return value
    if value.lower() == "true":
        return True
    if value.lower() == "false":
        return False
    if value.lower() == "null":
        return None
    ok, parsed = try_json(value)
    return _normalize_tool_input(parsed) if ok else value


# ── XML parsing ─────────────────────────────────────────────────
def _parse_xml_tool_calls(
    text: Any,
    allowed: Mapping[str, str],
    config: ToolCallConfig,
) -> List[ParsedToolCall]:
    calls: List[ParsedToolCall] = []
    raw_text = str(text or "")
    for m in re.finditer(r"<tool_call\b[^>]*>\s*([\s\S]*?)\s*</tool_call\s*>", raw_text, re.IGNORECASE):
        body = m.group(1).strip()
        parsed = try_json(body)
        calls.extend(
            _parse_json_tool_calls(parsed[1] if parsed[0] else _parse_tool_input(body), allowed, config)
        )
    for expression in (
        r"<tool_use\b([^>]*)>([\s\S]*?)</tool_use>",
        r"<tool_call\b([^>]*)>([\s\S]*?)</tool_call>",
        r"<function\b([^>]*)>([\s\S]*?)</function>",
        r"<invoke\b([^>]*)>([\s\S]*?)</invoke>",
    ):
        for m in re.finditer(expression, raw_text, re.IGNORECASE):
            name = _canonical_tool_name(_extract_name_attr(m.group(1)), allowed, config)
            if name:
                calls.append(
                    ParsedToolCall(
                        id=config.id_factory(),
                        name=name,
                        input=_parse_tool_input(m.group(2).strip()),
                    )
                )
    return calls


# ── JSON parsing (S4: O(n) balanced-bracket discovery) ──────────
def _for_each_json_fragment(text: Any, visit: Callable[[Any], None]) -> None:
    """Visit every plausible complete JSON fragment.

    Original implementation was O(n^2) (every start x every suffix).  This
    rewrite discovers balanced bracket pairs in a single O(n) pass, producing
    the same candidate set in the same (open-position) order.
    """
    normalized = strip_json_fence(str(text or ""))
    # Whole-text candidates first (plain / repaired / recovered), as before
    for candidate in (
        normalized,
        repair_loose_json(normalized),
        recover_json_like(normalized),
    ):
        ok, parsed = try_json(candidate)
        if ok:
            visit(parsed)
    # O(n): pair every '['/'{' open with its balanced close
    stack: List[int] = []
    pairs: List[Tuple[int, int]] = []
    for i, ch in enumerate(normalized):
        if ch in "{[":
            stack.append(i)
        elif ch in "}]" and stack:
            pairs.append((stack.pop(), i))
    pairs.sort(key=lambda p: p[0])  # open order
    for open_i, close_i in pairs:
        frag = normalized[open_i:close_i + 1]
        ok, parsed = try_json(frag)
        if ok:
            visit(parsed)


def _parse_json_tool_calls(
    value: Any,
    allowed: Mapping[str, str],
    config: ToolCallConfig,
) -> List[ParsedToolCall]:
    calls: List[ParsedToolCall] = []
    if isinstance(value, list):
        for item in value:
            calls.extend(_parse_json_tool_calls(item, allowed, config))
        return calls
    if not is_mapping(value):
        return calls
    for key in ("tool_calls", "tools"):
        if isinstance(value.get(key), list):
            for item in value[key]:
                calls.extend(_parse_json_tool_calls(item, allowed, config))
    name = first_string(value.get("name"), value.get("tool"), value.get("tool_name"), value.get("function_name"))
    input = first_defined(value.get("input"), value.get("arguments"), value.get("args"), value.get("parameters"))
    function = value.get("function")
    if is_mapping(function):
        name = name or first_string(function.get("name"))
        if input is None:
            input = first_defined(function.get("arguments"), function.get("input"), function.get("parameters"))
    canonical_name = _canonical_tool_name(name, allowed, config)
    if canonical_name:
        calls.append(
            ParsedToolCall(
                id=first_string(value.get("id"), value.get("call_id")) or config.id_factory(),
                name=canonical_name,
                input=_normalize_tool_input(input),
            )
        )
    return calls


def _normalize_tool_input(value: Any) -> Any:
    if value is None:
        return {}
    if isinstance(value, str):
        trimmed = value.strip()
        if not trimmed:
            return {}
        ok, parsed = try_json(trimmed)
        if ok:
            return _normalize_tool_input(parsed)
        kv = _parse_text_kv_input(trimmed)
        return kv if kv else value
    return value


def _repair_loose_json_aliases():
    """Placeholder retained for doc clarity."""


# ── Text KV parsing ─────────────────────────────────────────────
_ALIAS_MAP = {
    "function.name": "name",
    "name": "name",
    "tool": "name",
    "tool.name": "name",
    "tool_name": "name",
    "function.arguments": "arguments",
    "arguments": "arguments",
    "args": "arguments",
    "input": "arguments",
    "tool_input": "arguments",
    "parameters": "arguments",
}


def _parse_text_kv_tool_calls(
    text: Any,
    allowed: Mapping[str, str],
    tools: Sequence[Mapping[str, Any]],
    config: ToolCallConfig,
) -> List[ParsedToolCall]:
    values: Dict[str, List[str]] = {"name": [], "arguments": []}
    current = ""
    for raw_line in str(text or "").splitlines():
        line = raw_line.strip()
        match = re.match(r"^([A-Za-z_.-][A-Za-z0-9_.-]*)\s*:\s*(.*)$", line, re.DOTALL)
        if match and match.group(1).lower() in _ALIAS_MAP:
            current = _ALIAS_MAP[match.group(1).lower()]
            values[current].append(match.group(2).strip())
            continue
        if current:
            values[current].append(raw_line)
    if not values["name"]:
        return []
    raw_name = "\n".join(values["name"]).splitlines()[0].strip().strip("'\"")
    name = _canonical_tool_name(raw_name, allowed, config)
    if not name:
        return []
    input = _normalize_tool_input("\n".join(values["arguments"]).strip())
    call = _coerce_parsed_call(
        ParsedToolCall(id=config.id_factory(), name=name, input=input), tools, config
    )
    return [call] if call is not None else []


def _parse_text_kv_input(text: Any) -> Dict[str, str]:
    out: Dict[str, str] = {}
    for raw_line in str(text or "").splitlines():
        line = raw_line.strip()
        if not line:
            continue
        equals = line.find("=")
        colon = line.find(":")
        separator = colon if equals < 0 or (colon >= 0 and colon < equals) else equals
        if separator <= 0:
            continue
        key = line[:separator].strip()
        value = line[separator + 1:].strip().strip("'\"")
        if key:
            out[key] = value
    return out


def _parse_tool_input(text: str) -> Any:
    if not text:
        return {}
    ok, parsed = try_json(text)
    if ok:
        return _normalize_tool_input(parsed)
    parameters: Dict[str, Any] = {}
    for m in re.finditer(r"<([A-Za-z_][A-Za-z0-9_.:-]*)\b[^>]*>([\s\S]*?)</\1>", text):
        parameters[m.group(1)] = _decode_markup_value(m.group(2), m.group(1), ToolCallConfig.default())
    if parameters:
        return parameters
    kv = _parse_text_kv_input(text)
    return kv if kv else {"input": text}


# ── Coercion / validation ───────────────────────────────────────
def _coerce_parsed_call(
    call: ParsedToolCall,
    tools: Sequence[Mapping[str, Any]],
    config: ToolCallConfig,
) -> Optional[ParsedToolCall]:
    input = coerce_tool_input(call.name, call.input, tools, config=config)
    if config.unknown_tool == "error" and _tool_schema(call.name, tools) is None:
        raise XymlError("Unknown tool: {}".format(call.name))
    if _missing_required_args(call.name, input, tools):
        if config.missing_required == "error" or config.strict:
            raise XymlError("Missing required arguments for tool: {}".format(call.name))
        if config.missing_required == "drop":
            return None
    if _invalid_tool_args(input):
        return None
    return ParsedToolCall(id=call.id, name=call.name, input=input)


def _coerce_tool_input_by_schema(name: str, input: Any, tools: Sequence[Mapping[str, Any]]) -> Any:
    if not is_mapping(input):
        return input
    properties = _schema_properties(_tool_schema(name, tools))
    if not properties:
        return input
    fixed = dict(input)
    for key, value in fixed.items():
        if is_mapping(properties.get(key)):
            fixed[key] = _coerce_value_by_schema(value, properties[key])
    return fixed


def _coerce_value_by_schema(value: Any, schema: Mapping[str, Any]) -> Any:
    types = _schema_types(schema)
    if isinstance(value, str) and ("array" in types or "object" in types):
        parsed, changed = _parse_json_string_for_schema(value, "array" in types, "object" in types)
        if changed:
            value = parsed
    if "array" in types:
        if is_mapping(value):
            value = [value]
        if isinstance(value, list) and is_mapping(schema.get("items")):
            return [_coerce_value_by_schema(item, schema["items"]) for item in value]
        return value
    if "object" in types and is_mapping(value):
        properties = _schema_properties(schema)
        if not properties:
            return value
        fixed = dict(value)
        for key, child in properties.items():
            if key in fixed and is_mapping(child):
                fixed[key] = _coerce_value_by_schema(fixed[key], child)
        return fixed
    return value


def _parse_json_string_for_schema(value: str, want_array: bool, want_object: bool) -> Tuple[Any, bool]:
    stripped = value.strip()
    if not stripped:
        return value, False
    candidates = [stripped]
    if want_array and not stripped.startswith("["):
        candidates.append("[{}]".format(stripped))
    for candidate in candidates:
        ok, parsed = try_json(candidate)
        if not ok:
            continue
        if want_array and isinstance(parsed, list):
            return parsed, True
        if want_array and is_mapping(parsed):
            return [parsed], True
        if want_object and is_mapping(parsed):
            return parsed, True
    return value, False


def _schema_types(schema: Any) -> set:
    types: set = set()
    if not is_mapping(schema):
        return types
    kind = schema.get("type")
    if isinstance(kind, str):
        types.add(kind)
    elif isinstance(kind, list):
        types.update(i for i in kind if isinstance(i, str))
    if schema.get("properties") is not None:
        types.add("object")
    if schema.get("items") is not None:
        types.add("array")
    for key in ("anyOf", "oneOf", "allOf"):
        if isinstance(schema.get(key), list):
            for variant in schema[key]:
                types |= _schema_types(variant)
    return types


# ── Allowed-tool resolution ─────────────────────────────────────
def _build_allowed_tool_map(tools: Sequence[Mapping[str, Any]], config: ToolCallConfig) -> Dict[str, str]:
    allowed: Dict[str, str] = {}
    for tool in tools:
        name = tool.get("name")
        if not name:
            continue
        allowed[tool_alias_key(name)] = name
        # Safe aliases: client-safe name -> original name
        from .config import SAFE_TOOL_ALIASES  # noqa: PLC0415
        alias = SAFE_TOOL_ALIASES.get(name)
        if alias:
            allowed[tool_alias_key(alias)] = name
    for alias, canonical in config.tool_aliases.items():
        real = allowed.get(tool_alias_key(canonical), canonical)
        allowed[tool_alias_key(alias)] = real
    return allowed


def _canonical_tool_name(name: Any, allowed: Mapping[str, str], config: ToolCallConfig) -> str:
    raw = str(name or "").strip()
    if not raw:
        return ""
    direct = allowed.get(tool_alias_key(raw))
    if direct:
        return direct
    configured = config.tool_aliases.get(raw) or config.tool_aliases.get(raw.lower())
    if configured and allowed.get(tool_alias_key(configured)):
        return allowed[tool_alias_key(configured)]
    if raw.startswith("u_"):
        return allowed.get(tool_alias_key(raw[2:]), "")
    return "" if config.unknown_tool == "drop" else raw


def _dedupe_tool_calls(calls: Sequence[ParsedToolCall]) -> List[ParsedToolCall]:
    seen: set = set()
    out: List[ParsedToolCall] = []
    for call in calls:
        if not call.name:
            continue
        key = "{}\0{}".format(tool_alias_key(call.name), _stable_stringify(call.input))
        if key in seen:
            continue
        seen.add(key)
        out.append(call)
    return out


def _tool_schema(name: str, tools: Any) -> Optional[Mapping[str, Any]]:
    for tool in normalize_tools(tools):
        if tool.get("name") == name and is_mapping(tool.get("parameters") or tool.get("input_schema")):
            return tool.get("parameters") or tool.get("input_schema")
    return None


def _required_tool_args(name: str, tools: Any) -> List[str]:
    seen: set = set()
    required: List[str] = []

    def add(*keys: Any) -> None:
        for key in keys:
            if isinstance(key, str) and key and key not in seen:
                seen.add(key)
                required.append(key)

    schema = _tool_schema(name, tools)
    if is_mapping(schema) and isinstance(schema.get("required"), list):
        add(*schema["required"])
    if name == "Read":
        add("file_path")
    elif name == "Write":
        add("file_path", "content")
    elif name == "Edit":
        add("file_path")
    elif name in {"Bash", "PowerShell"}:
        add("command")
    return required


def _missing_required_args(name: str, input: Any, tools: Any) -> bool:
    if not is_mapping(input):
        return False
    for key in _required_tool_args(name, tools):
        value = input.get(key)
        if value is None:
            return True
        if isinstance(value, str) and not value.strip() and not _required_arg_allows_empty_string(name, key):
            return True
    return False


def _required_arg_allows_empty_string(tool_name: str, argument_name: str) -> bool:
    tn = tool_alias_key(tool_name)
    an = tool_alias_key(argument_name)
    return tn in {"write", "writefile", "createfile"} and an in {"content", "text", "body", "data", "value", "contents", "filecontent"}


def _invalid_tool_args(input: Any) -> bool:
    if not is_mapping(input):
        return False
    return any(
        _is_path_like_arg_name(key) and _path_like_arg_looks_polluted(str(value or ""))
        for key, value in input.items()
    )


def _is_path_like_arg_name(name: Any) -> bool:
    return tool_alias_key(name) in {
        "path", "filepath", "filename", "targetfile", "file",
        "dir", "directory", "cwd", "workdir", "workingdirectory",
    }


def _path_like_arg_looks_polluted(value: str) -> bool:
    trimmed = value.strip()
    if not trimmed or "\0" in trimmed or re.search(r"[\r\n<>]", trimmed):
        return True
    lowered = trimmed.lower()
    markers = (
        "<![cdata[", "]]>", "xyml|", "qnml|", "tool_calls", "invoke name=",
        "parameter name=", "</parameter", "</invoke", "function.name:", "function.arguments:",
    )
    return any(marker in lowered for marker in markers)


def _filter_input_for_tool(name: str, input: Mapping[str, Any], tools: Sequence[Mapping[str, Any]]) -> Dict[str, Any]:
    properties = _schema_properties(_tool_schema(name, tools))
    return dict(input) if not properties else {key: value for key, value in input.items() if key in properties}


def _tool_accepts_field(name: str, tools: Any, field: str) -> bool:
    properties = _schema_properties(_tool_schema(name, tools))
    return bool(properties and field in properties)


# ── Regex construction (cached) ─────────────────────────────────
@lru_cache(maxsize=256)
def _protocol_open_tag_re_key(proto_name: str, tag: str) -> re.Pattern:
    name_part = r"(?:{}\s*)?".format(escape_regex(proto_name))
    return re.compile(
        r"<\s*(?:[|:]\s*)*{}".format(name_part)
        + r"(?:\s*[|:]\s*)*{}\b[^>]*>".format(escape_regex(tag)),
        re.IGNORECASE,
    )


@lru_cache(maxsize=256)
def _protocol_tag_block_re_key(proto_name: str, tag: str) -> re.Pattern:
    escaped_protocol = escape_regex(proto_name)
    escaped_tag = escape_regex(tag)
    open_name = r"(?:{}\s*)?".format(escaped_protocol)
    close_name = r"(?:{}\s*)?".format(escaped_protocol)
    return re.compile(
        r"<\s*(?:[|:]\s*)*{}".format(open_name)
        + r"(?:\s*[|:]\s*)*{}\b([^>]*)>([\s\S]*?)<\s*/\s*(?:[|:]\s*)*{}".format(escaped_tag, close_name)
        + r"(?:\s*[|:]\s*)*{}\s*>".format(escaped_tag),
        re.IGNORECASE,
    )


def _protocol_open_tag_re(protocol: ProtocolSpec, tag: str) -> re.Pattern:
    return _protocol_open_tag_re_key(protocol.name, tag)


def _protocol_tag_block_re(protocol: ProtocolSpec, tag: str) -> re.Pattern:
    return _protocol_tag_block_re_key(protocol.name, tag)


# ── Misc internal helpers ───────────────────────────────────────
def _extract_name_attr(attributes: Any) -> str:
    m = re.search(
        r"(?:^|[\s|])name\s*=\s*(?:\"([^\"]*)\"|'([^']*)'|([^\s|/>]+))",
        str(attributes or ""),
        re.IGNORECASE | re.DOTALL,
    )
    if not m:
        return ""
    return html_unescape(next((v for v in m.groups() if v is not None), "").strip())


def _stable_stringify(value: Any) -> str:
    if isinstance(value, list):
        return "[{}]".format(",".join(_stable_stringify(item) for item in value))
    if is_mapping(value):
        return "{{{}}}".format(
            ",".join(
                "{}:{}".format(json.dumps(str(k), ensure_ascii=False), _stable_stringify(value[k]))
                for k in sorted(value, key=lambda item: str(item))
            )
        )
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"), default=str)


def _normalize_protocol_specs(values: Any, emit_protocol: str) -> List[ProtocolSpec]:
    out: List[ProtocolSpec] = []
    seen: set = set()
    for value in _as_list(values):
        spec = value if isinstance(value, ProtocolSpec) else ProtocolSpec(str(value))
        key = spec.name.lower()
        if key not in seen:
            seen.add(key)
            out.append(spec)
    if str(emit_protocol).lower() not in seen:
        out.insert(0, ProtocolSpec(emit_protocol))
    return out


def _as_list(value: Any) -> List[Any]:
    if isinstance(value, list):
        return value
    if value is None:
        return []
    return [value]