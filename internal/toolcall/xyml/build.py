"""Tool instruction building and call rendering (XYML/QNML markup)."""

from __future__ import annotations

from typing import Any, Dict, List, Mapping, Optional, Union

from ._util import (
    as_list,
    call_value,
    clip,
    escape_xml,
    is_mapping,
    json_dumps,
    summarize_schema,
)
from .config import (
    DEFAULT_TOOL_ALIASES,
    SAFE_TOOL_ALIASES,
    ProtocolSpec,
    ToolCall,
    ToolCallConfig,
    random_call_id,
    _normalize_protocol_spec,
)

# ── Re-export for backward compat ───────────────────────────────
def normalize_tools(value: Any) -> List[Dict[str, Any]]:
    """Normalize OpenAI function specifications and plain tool specifications."""
    return _normalize_impl(value)


def _normalize_impl(value: Any) -> List[Dict[str, Any]]:
    """Normalize OpenAI function specifications and plain tool specifications."""
    out: List[Dict[str, Any]] = []
    for raw in as_list(value):
        if not is_mapping(raw):
            continue
        if raw.get("type") == "function" and is_mapping(raw.get("function")):
            out.append(dict(raw["function"]))
        elif isinstance(raw.get("name"), str) and raw["name"].strip():
            out.append(dict(raw))
    return out


def _safe_tool_name(name: Any) -> str:
    trimmed = str(name or "").strip()
    if not trimmed:
        return ""
    if trimmed in SAFE_TOOL_ALIASES:
        return SAFE_TOOL_ALIASES[trimmed]
    if any(alias.lower() == trimmed.lower() for alias in SAFE_TOOL_ALIASES.values()):
        return trimmed
    return trimmed if trimmed.startswith("u_") else "u_{}".format(trimmed)


def _example_input_from_tool(tool: Mapping[str, Any]) -> Dict[str, Any]:
    params = _schema_properties(tool.get("parameters") or tool.get("input_schema"))
    if not params:
        return {"ARG": "value"}
    example = {key: _example_value(schema) for key, schema in list(params.items())[:3]}
    return example or {"ARG": "value"}


def _example_value(schema: Any) -> Any:
    kinds = _schema_types(schema)
    if "array" in kinds:
        return []
    if "object" in kinds:
        return {}
    if "boolean" in kinds:
        return True
    if "number" in kinds or "integer" in kinds:
        return 1
    return "value"


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


def _schema_properties(schema: Any) -> Optional[Dict[str, Any]]:
    if is_mapping(schema) and is_mapping(schema.get("properties")):
        return schema.get("properties")
    return None


def build_tool_instructions(
    tools: Any,
    *,
    config: Optional[Union[ToolCallConfig, Mapping[str, Any]]] = None,
    protocol: Optional[Union[str, ProtocolSpec, Mapping[str, Any]]] = None,
) -> str:
    cfg = config or ToolCallConfig()
    if not isinstance(cfg, ToolCallConfig):
        cfg = ToolCallConfig(cfg)
    active_protocol = _normalize_protocol_spec(protocol or cfg.emit_protocol)

    normalized = _normalize_impl(tools)
    safe_tools = [dict(t, name=_safe_tool_name(t.get("name"))) for t in normalized]
    names = [t["name"] for t in safe_tools if t.get("name")]

    schemas = []
    for t in safe_tools:
        params = t.get("parameters") or t.get("input_schema") or {}
        schemas.append(
            "\n".join(
                (
                    "Action name: {}".format(t["name"]),
                    "Description: {}".format(clip(t.get("description", ""), 240)),
                    "Parameters: {}".format(summarize_schema(params)),
                )
            )
        )

    example_tools = safe_tools[:2] or [
        {
            "name": "TOOL_NAME",
            "parameters": {"type": "object", "properties": {"ARG": {"type": "string"}}},
        }
    ]
    examples = "\n\n".join(
        render_tool_call(tool["name"], _example_input_from_tool(tool), config=cfg, protocol=active_protocol)
        for tool in example_tools
    )

    accepted = ", ".join(spec.name for spec in cfg.parse_protocols)
    schema_block = (
        "You have access to these tools:\n\n{}\n\n".format("\n\n".join(schemas))
        if schemas
        else ""
    )
    defensive_rules = ""
    if cfg.prompt_style != "minimal":
        defensive_rules = """
RULES:
1. If a tool is needed, output a parseable {name} tool-call block. If no tool is needed, answer normally.
2. Use exact action names and parameter names from the schema.
3. Strings should use <![CDATA[...]]>; objects may use JSON or nested XML-like values; arrays may use JSON arrays or repeated <item> nodes.
4. Never emit empty required parameters. Ask normally if required information is unknown.
5. After a tool result, call another tool only if needed; otherwise answer normally.
6. Path-like parameters must contain only the path string, not prose or protocol fragments.
""".format(name=active_protocol.name)

    rendered_format = render_tool_call("TOOL_NAME", {"ARG": "value"}, config=cfg, protocol=active_protocol)

    return """=== {name} TOOL CALL PROTOCOL ===
{schema_block}Default protocol for new tool calls: {name}
Accepted parse protocols by this client: {accepted}
Available action names: {names}

FORMAT:
{rendered_format}
{defensive_rules}
CORRECT EXAMPLES:

{examples}

Remember: the preferred tool-call form is <|{name}|tool_calls>...</|{name}|tool_calls>.
=== END {name} TOOL INSTRUCTIONS ===""".format(
        name=active_protocol.name,
        schema_block=schema_block,
        accepted=accepted,
        names=", ".join(names),
        rendered_format=rendered_format,
        defensive_rules=defensive_rules,
        examples=examples,
    )


# ── Rendering ───────────────────────────────────────────────────
def render_tool_call(
    name: Any,
    input: Any = None,
    *,
    config: Optional[Union[ToolCallConfig, Mapping[str, Any]]] = None,
    protocol: Optional[Union[str, ProtocolSpec, Mapping[str, Any]]] = None,
) -> str:
    cfg = config or ToolCallConfig()
    if not isinstance(cfg, ToolCallConfig):
        cfg = ToolCallConfig(cfg)
    active_protocol = _normalize_protocol_spec(protocol or cfg.emit_protocol)

    call_name = str(name or "").strip()
    if not call_name:
        return ""
    arguments = dict(input) if is_mapping(input) else {"input": input}

    protocol_name = active_protocol.name
    root = active_protocol.tags["root"]
    invoke = active_protocol.tags["invoke"]
    parameter = active_protocol.tags["parameter"]

    lines = [
        "<|{}|{}>".format(protocol_name, root),
        '  <|{}|{} name="{}">'.format(protocol_name, invoke, escape_xml(call_name)),
    ]
    for key in sorted(arguments, key=lambda item: str(item)):
        lines.append(
            '    <|{}|{} name="{}">{}</|{}|{}>'.format(
                protocol_name,
                parameter,
                escape_xml(key),
                _render_markup_value(arguments[key]),
                protocol_name,
                parameter,
            )
        )
    lines.extend(
        (
            "  </|{}|{}>".format(protocol_name, invoke),
            "</|{}|{}>".format(protocol_name, root),
        )
    )
    return "\n".join(lines)


def render_tool_calls(
    calls: Any,
    *,
    config: Optional[Union[ToolCallConfig, Mapping[str, Any]]] = None,
    protocol: Optional[Union[str, ProtocolSpec, Mapping[str, Any]]] = None,
) -> str:
    cfg = config or ToolCallConfig()
    if not isinstance(cfg, ToolCallConfig):
        cfg = ToolCallConfig(cfg)
    return "\n\n".join(
        rendered
        for rendered in (
            render_tool_call(
                call_value(c, "name"),
                call_value(c, "input", {}),
                config=cfg,
                protocol=protocol,
            )
            for c in as_list(calls)
        )
        if rendered
    )


def _render_markup_value(value: Any) -> str:
    if value is None:
        return "null"
    if isinstance(value, str):
        return "<![CDATA[{}]]>".format(value.replace("]]>", "]]]]><![CDATA[>"))
    if isinstance(value, bool):
        return "true" if value else "false"
    return json_dumps(value)