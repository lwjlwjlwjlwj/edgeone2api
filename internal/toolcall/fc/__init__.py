"""FC policy layer for prompt-based function calling (ported from toolforge app/fc).

Path B pipeline:
  inject.build_instructions   → XYML instruction block for the system prompt
  inject.render_history_messages → flatten OpenAI tool history into chat text
  parse.parse_text_to_calls   → normalized ToolCall list from model output
  parse.to_openai_tool_calls  → OpenAI `tool_calls` message shape
  recovery.*                  → truncation detection + corrective retry hints
  profiles.*                  → CLI tool-profile detection / few-shot blocks
  policy.resolve_fc_mode      → native passthrough vs prompt FC decision
"""

from . import inject, parse, policy, profiles, recovery
from .inject import (
    build_instructions,
    inject_prompt_messages,
    render_history_messages,
    strip_tools_from_openai_body,
    tools_for_sdk,
)
from .parse import (
    create_sieve,
    parse_text_to_calls,
    sieve_events_to_calls,
    strip_think_tags,
    to_openai_tool_calls,
)
from .policy import apply_policy, resolve_fc_mode
from .profiles import (
    ToolProfile,
    build_few_shot_block,
    detect_tool_profile,
    profile_instruction_block,
)
from .recovery import (
    build_retry_user_message,
    is_tool_call_truncated,
    parse_with_recovery_hint,
)

__all__ = [
    "inject",
    "parse",
    "policy",
    "profiles",
    "recovery",
    "build_instructions",
    "inject_prompt_messages",
    "render_history_messages",
    "strip_tools_from_openai_body",
    "tools_for_sdk",
    "create_sieve",
    "parse_text_to_calls",
    "sieve_events_to_calls",
    "strip_think_tags",
    "to_openai_tool_calls",
    "apply_policy",
    "resolve_fc_mode",
    "ToolProfile",
    "build_few_shot_block",
    "detect_tool_profile",
    "profile_instruction_block",
    "build_retry_user_message",
    "is_tool_call_truncated",
    "parse_with_recovery_hint",
]