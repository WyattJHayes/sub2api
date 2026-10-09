from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import time
from pathlib import Path

import pytest


@pytest.mark.skipif(sys.platform == "win32", reason="Worker deployment uses POSIX signals")
@pytest.mark.parametrize("mode", ["runner", "grader", "statistics"])
@pytest.mark.parametrize("phase", ["idle", "active", "claiming"])
@pytest.mark.parametrize("stop_signal", [signal.SIGTERM, signal.SIGINT])
def test_cli_stops_claiming_and_drains_held_lease(tmp_path, mode, phase, stop_signal):
    """Catch missing CLI handlers, blocked idle polling and cancelled lease results."""
    tests = Path(__file__).resolve().parent
    env = dict(os.environ)
    env["PYTHONPATH"] = os.pathsep.join((str(tests.parent / "src"), str(tests)))
    process = subprocess.Popen(
        [sys.executable, str(tests / "shutdown_probe.py"), mode, phase, str(tmp_path)],
        env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
    )
    try:
        deadline = time.monotonic() + 10
        while not (tmp_path / "ready").exists() and process.poll() is None:
            if time.monotonic() >= deadline:
                pytest.fail("Worker never reached controlled operation")
            time.sleep(0.01)
        assert (tmp_path / "ready").exists(), process.communicate(timeout=1)
        process.send_signal(stop_signal)
        if phase != "idle":
            time.sleep(0.1)
            assert process.poll() is None, "Signal aborted an active or newly acquired lease"
            (tmp_path / "release").touch()
        try:
            stdout, stderr = process.communicate(timeout=3)
        except subprocess.TimeoutExpired:
            pytest.fail("Worker did not exit promptly after draining")
        assert process.returncode == 0, stdout + stderr
        assert (tmp_path / "closed").exists()
        result = json.loads((tmp_path / "result.json").read_text(encoding="utf-8"))
        assert result["claims"] == 1
        assert result["failure_count"] == 0
        if phase == "idle":
            assert result["evidence_count"] == 0
            assert result["results"] == []
        elif mode == "runner":
            assert result["evidence_count"] == 1
            assert result["state_records"] == 0
        elif mode == "grader":
            assert result["results"] == [{"passed": True, "score": "1"}]
        else:
            assert result["results"] == [{"quality_report": True}]
    finally:
        if process.poll() is None:
            process.kill()
        process.communicate(timeout=3)
