from __future__ import annotations

import re
from collections.abc import Iterable
from dataclasses import dataclass
from pathlib import Path


@dataclass(frozen=True)
class ScanReport:
    blocked: bool
    rule_ids: tuple[str, ...]
    redacted_count: int


class ArtifactScanner:
    def __init__(self, internal_domains: Iterable[str] = ()) -> None:
        self.internal_domains = tuple(domain.lower().lstrip(".") for domain in internal_domains)
        self._rules: tuple[tuple[str, re.Pattern[str], bool], ...] = (
            ("aws_access_key", re.compile(r"\bAKIA[0-9A-Z]{16}\b"), True),
            ("jwt", re.compile(r"\beyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\b"), True),
            ("private_key", re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----"), True),
            ("email", re.compile(r"\b[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}\b"), False),
            ("phone", re.compile(r"\b(?:\+?\d[\d ()-]{7,}\d)\b"), False),
        )

    def scan(self, paths: Iterable[Path]) -> ScanReport:
        rule_ids: set[str] = set()
        redacted = 0
        for path in paths:
            try:
                text = path.read_text(encoding="utf-8", errors="replace")
            except OSError:
                continue
            for rule_id, pattern, blocked in self._rules:
                matches = pattern.findall(text)
                if matches:
                    rule_ids.add(rule_id)
                    if not blocked:
                        redacted += len(matches)
            lowered = text.lower()
            if any(domain and domain in lowered for domain in self.internal_domains):
                rule_ids.add("internal_domain")
        blocked = any(
            rule_id in {"aws_access_key", "jwt", "private_key", "internal_domain"}
            for rule_id in rule_ids
        )
        return ScanReport(
            blocked=blocked, rule_ids=tuple(sorted(rule_ids)), redacted_count=redacted
        )
