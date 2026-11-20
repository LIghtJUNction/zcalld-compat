#!/usr/bin/env python3
"""Exercise the real helper CLI with a synthetic snapshot and no network work."""
from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

HELPER = Path(sys.argv[1]).resolve()
sys.argv[:] = [sys.argv[0]]
ACCOUNT = "builtin:codex#0123456789abcdefabcd"


class MaintenanceCLI(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="zcalld-maintenance-cli-")
        self.addCleanup(self.temporary.cleanup)
        self.home = Path(self.temporary.name)
        self.root = self.home / "extensions"
        self.snapshot = self.home / "snapshot"
        self.env = os.environ.copy()
        self.env.update(HOME=str(self.home), XDG_CONFIG_HOME=str(self.home / "config"),
                        XDG_DATA_HOME=str(self.home / "data"), XDG_STATE_HOME=str(self.home / "state"))

    def command(self, *args, ok=True):
        result = subprocess.run([str(HELPER), "--root", str(self.root), "maintenance", *args],
                                env=self.env, capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode == 0, ok, (result.returncode, result.stdout, result.stderr))
        self.assertNotIn("NEVER-COPY-SNAPSHOT-SECRET", result.stdout + result.stderr)
        return json.loads(result.stdout) if ok else result

    def write(self, path, value):
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        path.write_text(json.dumps(value) + "\n")
        path.chmod(0o600)

    def fixture(self):
        original = self.home / "original-snapshot-owner"
        self.write(self.snapshot / "settings.json", {"codexWarmup": "week"})
        self.write(self.snapshot / "logins.json", [{"agent": "codex", "id": "synthetic-login", "user": "Synthetic@example.invalid", "secret": "NEVER-COPY-SNAPSHOT-SECRET"}])
        self.write(self.snapshot / "codex-warmup.json", {})
        self.write(self.root / "registry.json", {"version": 1, "plugins": [], "accounts": {ACCOUNT: {"enabled": True}}})
        self.write(self.root / "builtin-auth-map.json", {"version": 1, "source": str(original), "accounts": [{"key": ACCOUNT, "agent": "codex", "enabled": True, "sources": [{"kind": "logins", "path": str(original / "logins.json"), "login_id": "synthetic-login", "user_hash": hashlib.sha256(b"Synthetic@example.invalid").hexdigest()}]}]})
        self.write(self.root / "plugin-auth.json", {"unchanged": "TARGET-AUTH-CANARY"})

    def test_status_and_disabled_run_are_local_and_do_not_create_state(self):
        status = self.command("status")
        self.assertFalse(status["enabled"])
        self.assertTrue(status["local_only"])
        self.assertEqual(self.command("run-once")["status"], "disabled")
        self.assertFalse(self.root.exists())

    def test_review_apply_enable_not_due_disable_preserves_auth(self):
        self.fixture()
        auth = (self.root / "plugin-auth.json").read_bytes()
        registry = (self.root / "registry.json").read_bytes()
        plan = self.command("plan", "--source", str(self.snapshot))
        self.assertEqual(plan["weekly_warmup"], 1)
        self.assertFalse(plan["blocked"])
        result = self.command("apply", "--source", str(self.snapshot), "--expect-plan", plan["plan_hash"])
        self.assertFalse(result["enabled"])
        self.assertFalse(result["credentials_changed"])
        self.assertTrue(self.command("enable")["enabled"])
        result = self.command("run-once")
        self.assertTrue(result["enabled"])
        self.assertEqual(result["results"], [])
        self.assertFalse((self.root / "runtime").exists())
        self.assertFalse(self.command("disable")["enabled"])
        self.assertEqual(self.command("run-once")["status"], "disabled")
        self.assertEqual((self.root / "plugin-auth.json").read_bytes(), auth)
        self.assertEqual((self.root / "registry.json").read_bytes(), registry)

    def test_changed_snapshot_invalidates_exact_plan(self):
        self.fixture()
        plan = self.command("plan", "--source", str(self.snapshot))
        with (self.snapshot / "settings.json").open("a") as stream:
            stream.write("\n")
        self.command("apply", "--source", str(self.snapshot), "--expect-plan", plan["plan_hash"], ok=False)
        self.assertFalse((self.root / "maintenance").exists())

    def test_missing_snapshot_and_unknown_commands_are_errors(self):
        self.command("plan", ok=False)
        self.command("enable", ok=False)
        self.command("invented", ok=False)
        self.command("run-once", "--source", str(self.snapshot), ok=False)


if __name__ == "__main__":
    unittest.main()
