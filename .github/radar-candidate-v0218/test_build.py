"""Boundary tests for the fixed-source candidate build driver."""
import importlib.util
import sys
import unittest
from pathlib import Path

DRIVER = Path(__file__).with_name("build.py")


class CandidateBoundaries(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if DRIVER.exists():
            spec = importlib.util.spec_from_file_location("candidate_build", DRIVER)
            cls.driver = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(cls.driver)

    def setUp(self):
        self.assertTrue(DRIVER.exists(), "fixed-source candidate driver is missing")

    def test_resource_gate_keeps_reserve(self):
        d = self.driver
        d.validate_resources(12 * d.GIB, 7 * d.GIB, initial=True)
        with self.assertRaisesRegex(ValueError, "disk"):
            d.validate_resources(12 * d.GIB - 1, 7 * d.GIB, initial=True)
        with self.assertRaisesRegex(ValueError, "disk"):
            d.validate_resources(2 * d.GIB - 1, 7 * d.GIB, initial=False)
        with self.assertRaisesRegex(ValueError, "memory"):
            d.validate_resources(12 * d.GIB, 256 * 1024**2 - 1, initial=False)

    def test_version_command_preserves_stderr(self):
        output = self.driver.command([sys.executable, "-c", "import sys; print('0.2.18', file=sys.stderr)"],
                                     combined=True)
        self.assertEqual(output, "0.2.18")

    def record(self):
        d = self.driver
        return {"schema_version": "radar-v0218-image-record-v1", "version": "0.2.18",
                "revision": d.COMMIT, "source_commit": d.COMMIT,
                "source_sha256": d.SOURCE_HASH, "platform": "linux/amd64",
                **{role: {"repository": repo, "manifest_digest": "sha256:" + "a" * 64,
                          "config_digest": "sha256:" + "b" * 64}
                   for role, repo in d.REPOSITORIES.items()}}

    def test_record_binds_full_identity(self):
        d = self.driver
        d.validate_record(self.record())
        for field in ("revision", "source_commit", "source_sha256", "version", "platform"):
            record = self.record()
            record[field] = "wrong"
            with self.subTest(field=field), self.assertRaises(ValueError):
                d.validate_record(record)
        record = self.record()
        record["worker"]["repository"] = "ghcr.io/other/worker"
        with self.assertRaises(ValueError):
            d.validate_record(record)
        record = self.record()
        record["control_plane"]["manifest_digest"] = "latest"
        with self.assertRaises(ValueError):
            d.validate_record(record)

    def test_inspection_requires_platform_labels_and_config(self):
        d = self.driver
        item = self.record()["worker"]
        inspection = {"Id": item["config_digest"], "Os": "linux", "Architecture": "amd64",
                      "Config": {"Labels": {"org.opencontainers.image.revision": d.COMMIT,
                                             "io.sub2api.radar.source-sha256": d.SOURCE_HASH}}}
        d.validate_inspection(inspection, item)
        inspection["Architecture"] = "arm64"
        with self.assertRaises(ValueError):
            d.validate_inspection(inspection, item)
        inspection["Architecture"] = "amd64"
        inspection["Config"]["Labels"]["io.sub2api.radar.source-sha256"] = "c" * 64
        with self.assertRaises(ValueError):
            d.validate_inspection(inspection, item)


if __name__ == "__main__":
    unittest.main()
