from __future__ import annotations

import hashlib
import json
import tempfile
import unittest
from pathlib import Path

from deploy.radar.migration_ledger import (
    LedgerError,
    audit_candidate,
    audit_runtime,
    candidate_manifest,
    checksum_text,
    read_manifest,
    read_name_list,
)


class MigrationLedgerTests(unittest.TestCase):
    def _write_sql(self, directory: Path, name: str, content: str) -> str:
        path = directory / name
        path.write_text(content, encoding="utf-8")
        return checksum_text(content)

    def test_285_baseline_plus_eight_new_migrations_is_293_with_legacy_entries(self) -> None:
        baseline = {
            "001_init.sql": "a" * 64,
            "202_add_radar_tracked_models.sql": "b" * 64,
            "207_scope_radar_tracked_models_by_tenant.sql": "c" * 64,
        }
        candidate = {
            "001_init.sql": "a" * 64,
            "221_group_model_pricing.sql": "1" * 64,
            "222_group_usage_daily_rollups.sql": "2" * 64,
            "223_group_usage_rollup_timezone.sql": "3" * 64,
            "224_user_platform_quotas_add_cn_providers.sql": "4" * 64,
            "225_backfill_codex_fingerprint_seed.sql": "5" * 64,
            "225_channel_model_time_pricing.sql": "6" * 64,
            "226_channel_monitor_quota_mode.sql": "7" * 64,
            "227_ops_model_not_found_sla_classification.sql": "8" * 64,
        }
        expected_new = sorted(set(candidate) - set(baseline))
        result = audit_candidate(
            baseline,
            candidate,
            expected_new=expected_new,
            legacy_entries=sorted(set(baseline) - set(candidate)),
        )
        self.assertTrue(result["ok"], result)
        self.assertEqual(result["expected_schema_migrations"], 11)
        self.assertEqual(result["candidate_pending_migrations"], expected_new)
        self.assertEqual(result["legacy_entries"], sorted(set(baseline) - set(candidate)))

    def test_checksum_drift_is_blocked_even_when_filename_and_count_match(self) -> None:
        baseline = {"001_init.sql": "a" * 64}
        candidate = {"001_init.sql": "b" * 64}
        result = audit_candidate(baseline, candidate, expected_new=[], legacy_entries=[])
        self.assertFalse(result["ok"])
        self.assertEqual(result["checksum_mismatches"], ["001_init.sql"])

    def test_unknown_filename_is_blocked_even_when_total_is_expected(self) -> None:
        baseline = {"001_init.sql": "a" * 64, "002_users.sql": "b" * 64}
        candidate = {
            "001_init.sql": "a" * 64,
            "002_users.sql": "b" * 64,
            "999_unknown.sql": "c" * 64,
        }
        result = audit_candidate(
            baseline,
            candidate,
            expected_new=["003_expected.sql"],
            legacy_entries=[],
        )
        self.assertFalse(result["ok"])
        self.assertEqual(result["unknown_candidate_files"], ["999_unknown.sql"])
        self.assertEqual(result["candidate_pending_migrations"], ["999_unknown.sql"])

    def test_runtime_manifest_requires_exact_checksums_and_expected_count(self) -> None:
        baseline = {"001_init.sql": "a" * 64}
        candidate = {"001_init.sql": "a" * 64, "002_expected.sql": "b" * 64}
        actual = {**baseline, **{"002_expected.sql": "b" * 64}}
        result = audit_runtime(
            baseline,
            candidate,
            actual,
            expected_new=["002_expected.sql"],
            legacy_entries=[],
        )
        self.assertTrue(result["ok"], result)
        self.assertEqual(result["actual_schema_migrations"], 2)

        tampered = {**actual, "003_unknown.sql": "c" * 64}
        blocked = audit_runtime(
            baseline,
            candidate,
            tampered,
            expected_new=["002_expected.sql"],
            legacy_entries=[],
        )
        self.assertFalse(blocked["ok"])
        self.assertEqual(blocked["runtime_unknown_files"], ["003_unknown.sql"])

    def test_runtime_audit_emits_manifest_bound_ledger_identities(self) -> None:
        baseline = {
            "001_init.sql": "a" * 64,
            "002_legacy.sql": "b" * 64,
        }
        candidate = {
            "001_init.sql": "a" * 64,
            "003_expected.sql": "c" * 64,
        }
        actual = {
            **baseline,
            "003_expected.sql": "c" * 64,
        }

        result = audit_runtime(
            baseline,
            candidate,
            actual,
            expected_new=["003_expected.sql"],
            legacy_entries=["002_legacy.sql"],
        )

        self.assertTrue(result["ok"], result)
        self.assertEqual(result["baseline_schema_migrations"], 2)
        self.assertEqual(result["candidate_file_count"], 2)
        self.assertEqual(result["expected_schema_migrations"], 3)
        self.assertEqual(result["actual_schema_migrations"], 3)
        self.assertEqual(result["legacy_entries"], ["002_legacy.sql"])
        self.assertEqual(result["candidate_pending_migrations"], [])
        self.assertEqual(result["checksum_mismatches"], [])
        self.assertEqual(result["runtime_unknown_files"], [])
        self.assertEqual(result["runtime_checksum_mismatches"], [])
        for key in (
            "baseline_ledger_sha256",
            "candidate_ledger_sha256",
            "expected_candidate_ledger_sha256",
            "expected_runtime_ledger_sha256",
            "runtime_ledger_sha256",
        ):
            self.assertRegex(result[key], r"^[0-9a-f]{64}$")
        self.assertEqual(
            result["candidate_ledger_sha256"],
            result["expected_candidate_ledger_sha256"],
        )
        self.assertEqual(
            result["runtime_ledger_sha256"],
            result["expected_runtime_ledger_sha256"],
        )

    def test_duplicate_expected_or_legacy_names_are_rejected(self) -> None:
        baseline = {"001_init.sql": "a" * 64}
        candidate = {"001_init.sql": "a" * 64, "002_expected.sql": "b" * 64}
        with self.assertRaises(LedgerError):
            audit_candidate(
                baseline,
                candidate,
                expected_new=["002_expected.sql", "002_expected.sql"],
                legacy_entries=[],
            )
        with self.assertRaises(LedgerError):
            audit_candidate(
                baseline,
                candidate,
                expected_new=["002_expected.sql"],
                legacy_entries=["001_init.sql", "001_init.sql"],
            )

    def test_v01178_manifest_contract_has_285_baseline_8_new_and_2_legacy(self) -> None:
        root = Path(__file__).resolve().parents[2]
        manifest_dir = root / "deploy/radar/manifests/v0.1.178"
        baseline = read_manifest(manifest_dir / "migration-baseline.tsv")
        expected_new = read_name_list(manifest_dir / "expected-new.txt")
        legacy_entries = read_name_list(manifest_dir / "legacy-entries.txt")
        self.assertEqual(len(baseline), 285)
        self.assertEqual(len(expected_new), 8)
        self.assertEqual(len(legacy_entries), 2)
        complete_candidate = candidate_manifest(root / "backend/migrations")
        historical_names = (set(baseline) | set(expected_new)) - set(legacy_entries)
        candidate = {name: complete_candidate[name] for name in historical_names}
        result = audit_candidate(
            baseline,
            candidate,
            expected_new=expected_new,
            legacy_entries=legacy_entries,
        )
        self.assertTrue(result["ok"], result)
        self.assertEqual(result["expected_schema_migrations"], 293)
        self.assertEqual(result["candidate_file_count"], 291)
        self.assertEqual(result["unknown_candidate_files"], [])

    def test_v01181_manifest_contract_has_285_baseline_13_new_and_2_legacy(self) -> None:
        root = Path(__file__).resolve().parents[2]
        manifest_dir = root / "deploy/radar/manifests/v0.1.181"
        baseline = read_manifest(manifest_dir / "migration-baseline.tsv")
        expected_new = read_name_list(manifest_dir / "expected-new.txt")
        legacy_entries = read_name_list(manifest_dir / "legacy-entries.txt")
        self.assertEqual(len(baseline), 285)
        self.assertEqual(len(expected_new), 13)
        self.assertEqual(len(legacy_entries), 2)
        complete_candidate = candidate_manifest(root / "backend/migrations")
        historical_names = (set(baseline) | set(expected_new)) - set(legacy_entries)
        candidate = {name: complete_candidate[name] for name in historical_names}
        result = audit_candidate(
            baseline,
            candidate,
            expected_new=expected_new,
            legacy_entries=legacy_entries,
        )
        self.assertTrue(result["ok"], result)
        self.assertEqual(result["expected_schema_migrations"], 298)
        self.assertEqual(result["candidate_file_count"], 296)
        self.assertEqual(result["unknown_candidate_files"], [])

    def test_v01183_manifest_contract_has_285_baseline_15_new_and_2_legacy(self) -> None:
        root = Path(__file__).resolve().parents[2]
        manifest_dir = root / "deploy/radar/manifests/v0.1.183"
        baseline = read_manifest(manifest_dir / "migration-baseline.tsv")
        expected_new = read_name_list(manifest_dir / "expected-new.txt")
        legacy_entries = read_name_list(manifest_dir / "legacy-entries.txt")
        self.assertEqual(len(baseline), 285)
        self.assertEqual(len(expected_new), 15)
        self.assertEqual(len(legacy_entries), 2)
        complete_candidate = candidate_manifest(root / "backend/migrations")
        historical_names = (set(baseline) | set(expected_new)) - set(legacy_entries)
        candidate = {name: complete_candidate[name] for name in historical_names}
        result = audit_candidate(
            baseline,
            candidate,
            expected_new=expected_new,
            legacy_entries=legacy_entries,
        )
        self.assertTrue(result["ok"], result)
        self.assertEqual(result["expected_schema_migrations"], 300)
        self.assertEqual(result["candidate_file_count"], 298)
        self.assertEqual(result["unknown_candidate_files"], [])

    def test_v020_manifest_contract_has_285_baseline_22_new_and_2_legacy(self) -> None:
        root = Path(__file__).resolve().parents[2]
        manifest_dir = root / "deploy/radar/manifests/v0.2.0"
        baseline = read_manifest(manifest_dir / "migration-baseline.tsv")
        expected_new = read_name_list(manifest_dir / "expected-new.txt")
        legacy_entries = read_name_list(manifest_dir / "legacy-entries.txt")
        self.assertEqual(len(baseline), 285)
        self.assertEqual(len(expected_new), 22)
        self.assertEqual(len(legacy_entries), 2)
        result = audit_candidate(
            baseline,
            candidate_manifest(root / "backend/migrations"),
            expected_new=expected_new,
            legacy_entries=legacy_entries,
        )
        self.assertTrue(result["ok"], result)
        self.assertEqual(result["expected_schema_migrations"], 307)
        self.assertEqual(result["candidate_file_count"], 305)
        self.assertEqual(result["unknown_candidate_files"], [])


if __name__ == "__main__":
    unittest.main()
