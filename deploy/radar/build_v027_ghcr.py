#!/usr/bin/env python3
"""Build and record immutable Sub2API v0.2.7 Radar private GHCR candidates."""
from __future__ import annotations

import importlib.util
import re
import sys
from pathlib import Path


_BASE_PATH = Path(__file__).with_name("build_v025_ghcr.py")
_SPEC = importlib.util.spec_from_file_location("sub2api_radar_build_base", _BASE_PATH)
if _SPEC is None or _SPEC.loader is None:
    raise RuntimeError(f"cannot load release builder base: {_BASE_PATH}")
_BASE = importlib.util.module_from_spec(_SPEC)
sys.modules[_SPEC.name] = _BASE
_SPEC.loader.exec_module(_BASE)


_BASE.APP_VERSION = "0.2.7"
_BASE.SCHEMA_VERSION = "radar-v027-image-record-v1"
_BASE.IMAGE_TAG_RE = re.compile(r"^0\.2\.7-radar-v27-(\d{8}T\d{6}Z)$")

BuildInputs = _BASE.BuildInputs
validate_inputs = _BASE.validate_inputs
validate_current_source = _BASE.validate_current_source
main = _BASE.main


if __name__ == "__main__":
    raise SystemExit(main())
