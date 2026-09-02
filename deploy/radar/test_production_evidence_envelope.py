from __future__ import annotations

import importlib.util
import io
import json
import os
import stat
import sys
import tempfile
import unittest
from copy import deepcopy
from contextlib import redirect_stderr
from datetime import UTC, datetime
from pathlib import Path
from typing import Any


RADAR_DIR = Path(__file__).resolve().parent
MODULE_PATH = RADAR_DIR / "production_evidence_envelope.py"
SHA_A = "a" * 64
SHA_B = "b" * 64
SHA_C = "c" * 64
SHA_D = "d" * 64
RELEASE_A = "019f97ec-a8e1-78f0-b145-390533dff847"
RELEASE_B = "019f97ec-a8e1-78f0-b145-390533dff848"
OFFICIAL_V01178_COMMIT = "e0c48a19ed794a565e3858662520afe0a1f9f0ba"
NOW = datetime(2026, 8, 14, 12, 0, 0, tzinfo=UTC)
IMAGE_TAG = "0.1.178-radar-v16-20260816T021900Z"


def load_module() -> Any | None:
    if not MODULE_PATH.exists():
        return None
    spec = importlib.util.spec_from_file_location("radar_production_evidence_envelope", MODULE_PATH)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {MODULE_PATH.name}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


evidence = load_module()


def binding(**overrides: str) -> dict[str, str]:
    return {
        "candidate_image_record_sha256": SHA_A,
        "target_id": SHA_B,
        "host_fingerprint": SHA_C,
        **overrides,
    }


def candidate_record(**overrides: Any) -> dict[str, Any]:
    return {
        "schema_version": "radar-v01178-image-record-v1",
        "source_sha256": SHA_D,
        "revision": SHA_D[:40],
        "source_commit": SHA_D[:40],
        "version": "0.1.178",
        "image_tag": IMAGE_TAG,
        "build_date": "2026-08-14T11:00:00Z",
        "platform": "linux/amd64",
        "control_plane": {
            "repository": "ghcr.io/wyattjhayes/sub2api-radar-control-plane",
            "tag": "ghcr.io/wyattjhayes/sub2api-radar-control-plane:" + IMAGE_TAG,
            "manifest_digest": "sha256:" + "1" * 64,
            "config_digest": "sha256:" + "2" * 64,
            "version_output": "sub2api 0.1.178",
        },
        "worker": {
            "repository": "ghcr.io/wyattjhayes/sub2api-radar-worker",
            "tag": "ghcr.io/wyattjhayes/sub2api-radar-worker:" + IMAGE_TAG,
            "manifest_digest": "sha256:" + "3" * 64,
            "config_digest": "sha256:" + "4" * 64,
            "version_output": "0.1.178",
        },
        **overrides,
    }


class ProductionEvidenceEnvelopeTests(unittest.TestCase):
    def require_module(self) -> Any:
        self.assertIsNotNone(
            evidence,
            "production_evidence_envelope.py must provide the release evidence contract",
        )
        return evidence

    def valid_envelope(self, **overrides: Any) -> dict[str, Any]:
        module = self.require_module()
        return module.build_envelope(
            evidence_type="preflight",
            release_id=RELEASE_A,
            started_at="2026-08-13T11:00:00Z",
            finished_at="2026-08-13T11:01:00Z",
            binding=binding(),
            input_evidence_sha256={},
            payload={"migration_count": 286},
            **overrides,
        )

    def test_canonical_hash_is_stable_for_equivalent_objects(self) -> None:
        module = self.require_module()
        left = {"z": [3, 2, 1], "a": {"value": "radar"}}
        right = {"a": {"value": "radar"}, "z": [3, 2, 1]}
        self.assertEqual(module.canonical_json_bytes(left), module.canonical_json_bytes(right))
        self.assertEqual(module.canonical_sha256(left), module.canonical_sha256(right))

    def test_payload_input_and_envelope_tampering_are_rejected(self) -> None:
        module = self.require_module()
        document = self.valid_envelope()
        for field, replacement, message in (
            ("payload", {"migration_count": 284}, "payload_sha256"),
            ("input_evidence_sha256", {"backup": SHA_D}, "evidence_sha256"),
            ("evidence_sha256", SHA_D, "evidence_sha256"),
        ):
            with self.subTest(field=field):
                tampered = deepcopy(document)
                tampered[field] = replacement
                with self.assertRaisesRegex(ValueError, message):
                    module.validate_envelope(tampered, expected_type="preflight", now=NOW)

    def test_schema_identity_and_timestamp_violations_are_rejected(self) -> None:
        module = self.require_module()
        cases = (
            ({"schema_version": "unknown"}, "schema_version"),
            ({"release_id": "not-a-uuid"}, "release_id"),
            ({"started_at": "2026-08-14T11:02:00Z"}, "started_at"),
            ({"finished_at": "2026-08-14T13:02:00Z"}, "future"),
            ({"binding": {"candidate_image_record_sha256": SHA_A}}, "binding"),
            ({"unexpected": "value"}, "keys"),
        )
        for override, message in cases:
            with self.subTest(override=override):
                document = self.valid_envelope()
                document.update(override)
                with self.assertRaisesRegex(ValueError, message):
                    module.validate_envelope(document, expected_type="preflight", now=NOW)

    def test_predecessor_requires_shared_binding_hash_and_time_order(self) -> None:
        module = self.require_module()
        parent = self.valid_envelope()
        child = module.build_envelope(
            evidence_type="authorization",
            release_id=RELEASE_A,
            started_at="2026-08-13T11:02:00Z",
            finished_at="2026-08-13T11:03:00Z",
            binding=binding(),
            input_evidence_sha256={"preflight": parent["evidence_sha256"]},
            payload={"operator": "release-operator"},
        )
        module.validate_predecessor(child, "preflight", parent)

        for changed_binding, bad_time, message in (
            (binding(target_id=SHA_D), None, "target_id"),
            (binding(), "2026-08-13T10:59:00Z", "time"),
        ):
            with self.subTest(message=message):
                invalid = deepcopy(child)
                invalid["binding"] = changed_binding
                if bad_time:
                    invalid["started_at"] = bad_time
                invalid = module.build_envelope(
                    evidence_type=invalid["evidence_type"],
                    release_id=invalid["release_id"],
                    started_at=invalid["started_at"],
                    finished_at=invalid["finished_at"],
                    binding=invalid["binding"],
                    input_evidence_sha256=invalid["input_evidence_sha256"],
                    payload=invalid["payload"],
                )
                with self.assertRaisesRegex(ValueError, message):
                    module.validate_predecessor(invalid, "preflight", parent)

    def test_private_reader_rejects_insecure_files(self) -> None:
        module = self.require_module()
        with tempfile.TemporaryDirectory() as directory:
            private = Path(directory) / "private"
            private.mkdir(mode=0o700)
            document = self.valid_envelope()
            target = private / "preflight.json"
            target.write_text(json.dumps(document), encoding="utf-8")
            os.chmod(target, 0o644)
            with self.assertRaisesRegex(ValueError, "0600"):
                module.load_private_envelope(target, expected_type="preflight", now=NOW)

            os.chmod(target, 0o600)
            linked = private / "linked.json"
            linked.symlink_to(target)
            with self.assertRaisesRegex(ValueError, "symlink"):
                module.load_private_envelope(linked, expected_type="preflight", now=NOW)

    def test_candidate_image_record_rejects_unbound_source_and_insecure_file(self) -> None:
        module = self.require_module()
        with tempfile.TemporaryDirectory() as directory:
            private = Path(directory) / "private"
            private.mkdir(mode=0o700)
            record_path = private / "image-record.json"
            record_path.write_text(json.dumps(candidate_record()), encoding="utf-8")
            os.chmod(record_path, 0o600)
            loaded = module.load_candidate_image_record(record_path, expected_source_sha256=SHA_D)
            self.assertEqual(SHA_D[:40], loaded["revision"])

            record_path.write_text(
                json.dumps(candidate_record(revision=SHA_A[:40])), encoding="utf-8"
            )
            with self.assertRaisesRegex(ValueError, "source_commit"):
                module.load_candidate_image_record(record_path)

            record_path.write_text(
                json.dumps(candidate_record(source_commit=None)), encoding="utf-8"
            )
            with self.assertRaisesRegex(ValueError, "source_commit"):
                module.load_candidate_image_record(record_path)

            os.chmod(record_path, 0o644)
            with self.assertRaisesRegex(ValueError, "0600"):
                module.load_candidate_image_record(record_path)

    def test_candidate_image_record_accepts_git_revision_distinct_from_source_tree_hash(self) -> None:
        module = self.require_module()
        with tempfile.TemporaryDirectory() as directory:
            private = Path(directory) / "private"
            private.mkdir(mode=0o700)
            record_path = private / "image-record.json"
            record_path.write_text(
                json.dumps(
                    candidate_record(
                        revision=OFFICIAL_V01178_COMMIT,
                        source_commit=OFFICIAL_V01178_COMMIT,
                    )
                ),
                encoding="utf-8",
            )
            os.chmod(record_path, 0o600)

            loaded = module.load_candidate_image_record(record_path, expected_source_sha256=SHA_D)

            self.assertEqual(OFFICIAL_V01178_COMMIT, loaded["revision"])
            self.assertEqual(OFFICIAL_V01178_COMMIT, loaded["source_commit"])

    def test_candidate_image_record_rejects_legacy_schema_and_tag(self) -> None:
        module = self.require_module()
        with tempfile.TemporaryDirectory() as directory:
            private = Path(directory) / "private"
            private.mkdir(mode=0o700)
            record_path = private / "image-record.json"
            record_path.write_text(
                json.dumps(candidate_record(schema_version="radar-v01173-image-record-v1")),
                encoding="utf-8",
            )
            os.chmod(record_path, 0o600)
            with self.assertRaisesRegex(ValueError, "schema_version"):
                module.load_candidate_image_record(record_path)

            record_path.write_text(
                json.dumps(candidate_record(schema_version="radar-v01177-image-record-v1")),
                encoding="utf-8",
            )
            with self.assertRaisesRegex(ValueError, "schema_version"):
                module.load_candidate_image_record(record_path)

            old_tag = "0.1.173-radar-v13-20260814T110000Z"
            record_path.write_text(
                json.dumps(
                    candidate_record(
                        image_tag=old_tag,
                        control_plane={
                            **candidate_record()["control_plane"],
                            "tag": "ghcr.io/wyattjhayes/sub2api-radar-control-plane:" + old_tag,
                        },
                        worker={
                            **candidate_record()["worker"],
                            "tag": "ghcr.io/wyattjhayes/sub2api-radar-worker:" + old_tag,
                        },
                    )
                ),
                encoding="utf-8",
            )
            with self.assertRaisesRegex(ValueError, "image_tag"):
                module.load_candidate_image_record(record_path)

    def test_private_writer_creates_mode_0600_document(self) -> None:
        module = self.require_module()
        with tempfile.TemporaryDirectory() as directory:
            private = Path(directory) / "private"
            private.mkdir(mode=0o700)
            target = private / "preflight.json"
            module.write_private_json(target, self.valid_envelope())
            self.assertEqual(0o600, stat.S_IMODE(target.stat().st_mode))
            loaded = module.load_private_envelope(target, expected_type="preflight", now=NOW)
            self.assertEqual(286, loaded["payload"]["migration_count"])

            linked_private = Path(directory) / "linked-private"
            linked_private.symlink_to(private, target_is_directory=True)
            with self.assertRaisesRegex(ValueError, "symlink"):
                module.write_private_json(linked_private / "other.json", self.valid_envelope())


if __name__ == "__main__":
    unittest.main()
