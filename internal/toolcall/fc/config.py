"""Minimal policy config for the sidecar (subset of toolforge app/config.py).

The 2api gateway always uses Path B (prompt FC): tools are stripped from the
upstream body, XYML instructions are injected, and model output is parsed into
standard tool_calls.  These dataclasses keep the toolforge policy layer
self-contained so `resolve_fc_mode` can run unchanged.
"""

from __future__ import annotations

from dataclasses import dataclass, field


@dataclass
class UpstreamConfig:
    native_fc: bool = False


@dataclass
class FeaturesConfig:
    fc_mode: str = "auto"  # auto | prefer_native | force_prompt


@dataclass
class AppConfig:
    features: FeaturesConfig = field(default_factory=FeaturesConfig)