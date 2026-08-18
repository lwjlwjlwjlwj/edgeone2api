"""Protocol configuration — ProtocolSpec, ToolCallConfig, ToolCallEngine.

Snake-case option names are preferred in Python. JavaScript SDK option names
are also accepted so a shared JSON configuration can be reused unchanged.
"""

from __future__ import annotations

import secrets
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional, Set, Union

from ._util import is_mapping


# ── Defaults ────────────────────────────────────────────────────
DEFAULT_RAW_STRING_PARAMS: Set[str] = {
    "content", "command", "cmd", "script", "code", "prompt",
    "file_content", "old_string", "new_string", "insert_text",
    "patch", "pattern", "text", "query", "url", "path", "file_path",
}

DEFAULT_TOOL_ALIASES: Dict[str, str] = {
    "fs_open_file": "Read",
    "fs_put_file": "Write",
    "fs_patch_file": "Edit",
    "shell_run": "Bash",
    "text_search": "Grep",
    "path_find": "Glob",
    "notebook_patch": "NotebookEdit",
    "http_get_url": "WebFetch",
    "web_query": "WebSearch",
}

SAFE_TOOL_ALIASES: Dict[str, str] = {v: k for k, v in DEFAULT_TOOL_ALIASES.items()}


# ── Exceptions ──────────────────────────────────────────────────
class XymlError(ValueError):
    """Base exception for XYML engine errors."""


class UnknownToolError(XymlError):
    """Raised when unknown_tool = 'error' and a call references an unknown tool."""


class MissingRequiredError(XymlError):
    """Raised when missing_required = 'error' and required args are missing."""


# ── ID helpers ──────────────────────────────────────────────────
def random_id(length: int = 12) -> str:
    alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
    return "".join(secrets.choice(alphabet) for _ in range(length))


def random_call_id() -> str:
    return "call_{}".format(random_id(12))


# ── Core data types ─────────────────────────────────────────────
@dataclass
class ParsedToolCall:
    """A normalized function call extracted from model output."""
    name: str
    input: Any = field(default_factory=dict)
    id: str = field(default_factory=random_call_id)

    def __post_init__(self) -> None:
        if not self.id:
            self.id = random_call_id()
        if self.input is None:
            self.input = {}


# Canonical alias (kept for backward compat, same type)
ToolCall = ParsedToolCall


class ProtocolSpec:
    """Names and tags for one markup tool-call protocol."""

    def __init__(
        self,
        name: str,
        parse_only: bool = False,
        tags: Optional[Mapping[str, str]] = None,
        **options: Any,
    ) -> None:
        if "parseOnly" in options:
            parse_only = bool(options.pop("parseOnly"))
        if not isinstance(name, str) or not name.strip():
            raise TypeError("ProtocolSpec name must be a non-empty string")
        supplied_tags = dict(tags or options.pop("tags", {}) or {})
        self.name = name.strip()
        self.parse_only = bool(parse_only)
        self.tags = {
            "root": supplied_tags.get("root", "tool_calls"),
            "invoke": supplied_tags.get("invoke", "invoke"),
            "parameter": supplied_tags.get("parameter", "parameter"),
        }


class ToolCallConfig:
    """Parser, compatibility, and validation policies for tool calls.

    Always copies the input dict — never mutates caller data.
    """

    def __init__(self, options: Optional[Mapping[str, Any]] = None, **kwargs: Any) -> None:
        # Shallow copy to avoid S3‑style mutation of caller data
        values: Dict[str, Any] = dict(options or {})
        values.update(kwargs)

        self.emit_protocol = str(
            values.pop("emit_protocol",
                values.pop("emitProtocol",
                    values.pop("default_protocol",
                        values.pop("defaultProtocol", "XYML")))
            ) or "XYML"
        ).strip()

        protocols = values.pop("parse_protocols",
                       values.pop("parseProtocols",
                           values.pop("protocols", None)))
        if protocols is None:
            protocols = [
                ProtocolSpec(self.emit_protocol),
                ProtocolSpec("QNML", parse_only=True),
            ]
        self.parse_protocols = _normalize_protocol_specs(protocols, self.emit_protocol)

        self.strict = bool(values.pop("strict", False))
        self.unknown_tool = values.pop("unknown_tool",
                            values.pop("unknownTool", "drop"))
        self.missing_required = values.pop("missing_required",
                                values.pop("missingRequired", "drop"))
        self.enable_markup = bool(values.pop("enable_markup",
                            values.pop("enableMarkup", True)))
        self.enable_xml = bool(values.pop("enable_xml",
                            values.pop("enableXml", True)))
        self.enable_json = bool(values.pop("enable_json",
                            values.pop("enableJson", True)))
        self.enable_text_kv = bool(values.pop("enable_text_kv",
                               values.pop("enableTextKV", True)))
        self.enable_coercion = bool(values.pop("enable_coercion",
                                values.pop("enableCoercion", True)))
        self.enable_dedupe = bool(values.pop("enable_dedupe",
                              values.pop("enableDedupe", True)))
        self.prompt_style = values.pop("prompt_style",
                            values.pop("promptStyle", "standard"))
        self.tool_aliases = dict(DEFAULT_TOOL_ALIASES)
        self.tool_aliases.update(
            dict(values.pop("tool_aliases",
                 values.pop("toolAliases", {})) or {})
        )
        self.argument_aliases = dict(
            values.pop("argument_aliases",
                values.pop("argumentAliases", {})) or {}
        )
        custom_raw = values.pop("raw_string_params",
                      values.pop("rawStringParams", [])) or []
        self.raw_string_params = set(DEFAULT_RAW_STRING_PARAMS)
        self.raw_string_params.update(str(v).lower() for v in custom_raw)
        self.id_factory = values.pop("id_factory",
                          values.pop("idFactory", random_call_id))
        if not callable(self.id_factory):
            raise TypeError("id_factory must be callable")

    @classmethod
    def default(cls) -> "ToolCallConfig":
        return cls()

    def with_overrides(self, overrides: Optional[Mapping[str, Any]] = None, **kwargs: Any) -> "ToolCallConfig":
        values: Dict[str, Any] = {
            "emit_protocol": self.emit_protocol,
            "parse_protocols": self.parse_protocols,
            "strict": self.strict,
            "unknown_tool": self.unknown_tool,
            "missing_required": self.missing_required,
            "enable_markup": self.enable_markup,
            "enable_xml": self.enable_xml,
            "enable_json": self.enable_json,
            "enable_text_kv": self.enable_text_kv,
            "enable_coercion": self.enable_coercion,
            "enable_dedupe": self.enable_dedupe,
            "prompt_style": self.prompt_style,
            "tool_aliases": self.tool_aliases,
            "argument_aliases": self.argument_aliases,
            "raw_string_params": list(self.raw_string_params),
            "id_factory": self.id_factory,
        }
        values.update(dict(overrides or {}))
        values.update(kwargs)
        return ToolCallConfig(values)

    with_ = with_overrides


class ToolCallEngine:
    """Stateful facade around the standalone parser and renderer functions."""

    def __init__(
        self,
        options: Optional[Union["ToolCallConfig", Mapping[str, Any]]] = None,
        *,
        config: Optional[Union["ToolCallConfig", Mapping[str, Any]]] = None,
        **kwargs: Any,
    ) -> None:
        if config is not None:
            source: Optional[Union["ToolCallConfig", Mapping[str, Any]]] = config
        elif isinstance(options, Mapping) and "config" in options:
            source = options["config"]
        else:
            source = options
        if kwargs:
            merged = dict(source or {}) if isinstance(source, Mapping) else {}
            merged.update(kwargs)
            source = merged
        self.config = _resolve_config(source)

    @classmethod
    def default(cls) -> "ToolCallEngine":
        return cls()

    @classmethod
    def with_protocols(
        cls,
        emit: str = "XYML",
        parse: Optional[Sequence[Union[str, ProtocolSpec, Mapping[str, Any]]]] = None,
        **options: Any,
    ) -> "ToolCallEngine":
        parse = parse or ["XYML", "QNML"]
        protocols: List[ProtocolSpec] = []
        for item in parse:
            if isinstance(item, ProtocolSpec):
                protocols.append(item)
            else:
                protocols.append(ProtocolSpec(str(item), parse_only=str(item) != emit))
        options["emit_protocol"] = emit
        options["parse_protocols"] = protocols
        return cls(options)

    # Deferred imports to avoid circular dependency at module level
    def normalize_tools(self, tools: Any) -> List[Dict[str, Any]]:
        from .build import normalize_tools  # noqa: PLC0415
        return normalize_tools(tools)

    def build_instructions(
        self,
        tools: Any,
        *,
        config: Optional[Union["ToolCallConfig", Mapping[str, Any]]] = None,
        protocol: Optional[Union[str, ProtocolSpec, Mapping[str, Any]]] = None,
    ) -> str:
        from .build import build_tool_instructions  # noqa: PLC0415
        return build_tool_instructions(tools, config=config or self.config, protocol=protocol)

    def parse(
        self,
        text: Any,
        tools: Any,
        *,
        config: Optional[Union["ToolCallConfig", Mapping[str, Any]]] = None,
    ) -> List[ParsedToolCall]:
        from .parse import parse_tool_calls  # noqa: PLC0415
        return parse_tool_calls(text, tools, config=config or self.config)

    def render(
        self,
        call_or_name: Union[ParsedToolCall, Mapping[str, Any], str],
        input: Any = None,
        *,
        config: Optional[Union["ToolCallConfig", Mapping[str, Any]]] = None,
        protocol: Optional[Union[str, ProtocolSpec, Mapping[str, Any]]] = None,
    ) -> str:
        from .build import render_tool_call  # noqa: PLC0415
        active_config = config or self.config
        if isinstance(call_or_name, str):
            return render_tool_call(call_or_name, input, config=active_config, protocol=protocol)
        return render_tool_call(
            call_value(call_or_name, "name"),
            call_value(call_or_name, "input", {}),
            config=active_config,
            protocol=protocol,
        )

    def create_sieve(
        self,
        tools: Any,
        *,
        hold_length: int = 96,
        config: Optional[Union["ToolCallConfig", Mapping[str, Any]]] = None,
    ) -> "ToolSieve":
        from .sieve import ToolSieve  # noqa: PLC0415
        return ToolSieve(tools, config=config or self.config, hold_length=hold_length)


# ── Internal helpers ────────────────────────────────────────────
def _normalize_protocol_specs(values: Any, emit_protocol: str) -> List[ProtocolSpec]:
    out: List[ProtocolSpec] = []
    seen: Set[str] = set()
    for value in as_list(values):
        spec = _normalize_protocol_spec(value)
        key = spec.name.lower()
        if key not in seen:
            seen.add(key)
            out.append(spec)
    if str(emit_protocol).lower() not in seen:
        out.insert(0, ProtocolSpec(emit_protocol))
    return out


def _normalize_protocol_spec(value: Union[str, ProtocolSpec, Mapping[str, Any]]) -> ProtocolSpec:
    if isinstance(value, ProtocolSpec):
        return value
    if isinstance(value, str):
        return ProtocolSpec(value)
    if is_mapping(value):
        opts = dict(value)
        name = opts.pop("name", None)
        return ProtocolSpec(name, **opts)
    raise TypeError("Invalid protocol spec")


def _resolve_config(config: Optional[Union[ToolCallConfig, Mapping[str, Any]]]) -> ToolCallConfig:
    if isinstance(config, ToolCallConfig):
        return config
    if config is None:
        return ToolCallConfig()
    if is_mapping(config):
        return ToolCallConfig(config)
    raise TypeError("config must be a ToolCallConfig or mapping")


# Local imports for circular-safe references
from ._util import as_list, call_value  # noqa: E402, F811