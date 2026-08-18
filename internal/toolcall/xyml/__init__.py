"""Protocol-agnostic helpers for LLM tool calling.

This package is a structured refactoring of the original app/engine/xyml.py
monolith.  All public API symbols are re-exported here for backward compatibility.
"""

from __future__ import annotations

from ._util import (
    escape_xml,
    html_unescape,
    canonicalize_markup,
    strip_markdown_fences,
    strip_json_fence,
    try_json,
    is_mapping,
    as_list,
    call_value,
    first_string,
    first_defined,
    json_dumps,
    stable_stringify,
    tool_alias_key,
    clip,
    summarize_schema,
    rename_first_present,
    repair_loose_json,
    recover_json_like,
)

from .config import (
    DEFAULT_RAW_STRING_PARAMS,
    DEFAULT_TOOL_ALIASES,
    SAFE_TOOL_ALIASES,
    XymlError,
    UnknownToolError,
    MissingRequiredError,
    random_id,
    random_call_id,
    ParsedToolCall,
    ToolCall,
    ProtocolSpec,
    ToolCallConfig,
    ToolCallEngine,
)

from .build import (
    normalize_tools,
    build_tool_instructions,
    render_tool_call,
    render_tool_calls,
)

from .parse import (
    parse_tool_calls,
    parse_markup_tool_calls,
    coerce_tool_input,
)

from .sieve import (
    ToolSieve,
)

from .output import (
    openai_tool_calls,
    responses_tool_items,
    anthropic_tool_use_blocks,
)

# ── JS-compatible aliases for backward compatibility ────────────
normalizeTools = normalize_tools
buildToolInstructions = build_tool_instructions
renderToolCall = render_tool_call
renderToolCalls = render_tool_calls
parseToolCalls = parse_tool_calls
parseMarkupToolCalls = parse_markup_tool_calls
coerceToolInput = coerce_tool_input
openAIToolCalls = openai_tool_calls
responsesToolItems = responses_tool_items
anthropicToolUseBlocks = anthropic_tool_use_blocks

# ToolRuntime was removed per S1 (dead code — middleware never executes tools).

__all__ = [
    "DEFAULT_RAW_STRING_PARAMS", "DEFAULT_TOOL_ALIASES", "SAFE_TOOL_ALIASES",
    "XymlError", "UnknownToolError", "MissingRequiredError",
    "random_id", "random_call_id",
    "ParsedToolCall", "ToolCall",
    "ProtocolSpec", "ToolCallConfig", "ToolCallEngine",
    "normalize_tools", "build_tool_instructions",
    "render_tool_call", "render_tool_calls",
    "parse_tool_calls", "parse_markup_tool_calls", "coerce_tool_input",
    "openai_tool_calls", "responses_tool_items", "anthropic_tool_use_blocks",
    "ToolSieve",
    # JS aliases
    "normalizeTools", "buildToolInstructions",
    "renderToolCall", "renderToolCalls",
    "parseToolCalls", "parseMarkupToolCalls",
    "coerceToolInput", "openAIToolCalls",
    "responsesToolItems", "anthropicToolUseBlocks",
]