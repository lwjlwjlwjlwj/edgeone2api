"""Protocol-neutral request/response models (ported from toolforge app/models)."""

from .canonical import (
    CanonicalRequest,
    CanonicalResponse,
    FCMode,
    Message,
    ResolvedFCMode,
    Surface,
    ToolCall,
    ToolDef,
    openai_messages_to_canonical,
    openai_tools_to_defs,
    tool_defs_to_openai,
)

__all__ = [
    "Surface",
    "FCMode",
    "ResolvedFCMode",
    "ToolCall",
    "ToolDef",
    "Message",
    "CanonicalRequest",
    "CanonicalResponse",
    "tool_defs_to_openai",
    "openai_tools_to_defs",
    "openai_messages_to_canonical",
]