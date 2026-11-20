#!/usr/bin/env python3
"""Real pluginctl, synthetic native profiles and fake Claude; no network or accounts."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--helper", required=True, type=Path)
    parser.add_argument("--fake-claude", required=True, type=Path)
    args = parser.parse_args()
    helper, fake = args.helper.resolve(strict=True), args.fake_claude.resolve(strict=True)
    results = []
    with tempfile.TemporaryDirectory(prefix="zcalld-claude-cli-") as temporary:
        base = Path(temporary).resolve()
        env = os.environ.copy()
        env.update(HOME=str(base), USERPROFILE=str(base), XDG_CONFIG_HOME=str(base / "config"),
                   XDG_DATA_HOME=str(base / "data"), XDG_STATE_HOME=str(base / "state"),
                   APPDATA=str(base / "appdata"), LOCALAPPDATA=str(base / "localappdata"))

        def protect(*paths):
            subprocess.run([str(fake), "--protect-fixture", *map(str, paths)],
                           env=env, check=True, capture_output=True, timeout=10)

        def mkdir(path):
            path.mkdir(mode=0o700, parents=True, exist_ok=True)
            protect(path)
            return path

        def write(path, value):
            path.write_text(json.dumps(value) + "\n", encoding="utf-8")
            protect(path)

        protect(base)

        def call(root, *argv, success=True):
            process = subprocess.run([str(helper), "--root", str(root), "maintenance", *argv, "--json"],
                                     env=env, capture_output=True, text=True, timeout=65)
            if success:
                assert process.returncode == 0, (argv, process.stderr)
                return json.loads(process.stdout)
            assert process.returncode != 0, argv
            return process

        def fixture(name, print_mode=False, native=False, no_extra=False):
            directory = mkdir(base / name)
            root = mkdir(directory / "extensions")
            snapshot = mkdir(directory / "snapshot")
            home = mkdir(directory / "native-home")
            config = mkdir(home / ".claude" if native else directory / "saved-account")
            profile = home / ".claude.json" if native else config / ".claude.json"
            identity = {"emailAddress": "synthetic@example.invalid", "organizationUuid": "fake-org", "accountUuid": "fake-member"}
            write(profile, {"oauthAccount": identity, "unrelated": "preserved"})
            write(snapshot / "settings.json", {"claudeWarmup": "week", "claudePrintMode": print_mode, "claudeNoExtraUsage": no_extra})
            write(snapshot / "logins.json", [{"id": "fake-login", "agent": "claude", "user": identity["emailAddress"],
                                               "profile": identity, "on": True, "auth": {"secret": "SYNTHETIC-NEVER-IMPORT"}}])
            write(snapshot / "providers.json", {"providers": [{"id": "claude"}]})
            write(snapshot / "claude-warmup.json", {})
            binding = directory / "bindings.json"
            write(binding, {"version": 1, "claude_binary": str(fake), "interactive_helper": str(fake), "accounts": [{
                "login_id": "fake-login", "config_dir": str(config), "profile": str(profile),
                "ownership": "current" if native else "saved", "enabled": True}]})
            return root, snapshot, config, profile, binding

        def apply(item):
            root, snapshot, config, profile, binding = item
            plan = call(root, "plan", "--provider", "claude", "--source", str(snapshot), "--file", str(binding))
            assert plan["accounts"] == 1 and not plan["applied"] and not plan["enabled"]
            assert not (config / "generations.jsonl").exists()
            applied = call(root, "apply", "--provider", "claude", "--source", str(snapshot), "--file", str(binding), "--expect-plan", plan["plan_hash"])
            assert applied["applied"] and not applied["enabled"] and not applied["credentials_imported"] and not applied["scheduler_changed"]
            assert "SYNTHETIC-NEVER-IMPORT" not in (root / "maintenance/claude/state.json").read_text()
            return applied["account_ids"][0]

        root = mkdir(base / "disabled")
        status = call(root, "status", "--provider", "claude")
        report = call(root, "run-once", "--provider", "claude")
        assert not status["status"]["enabled"] and report["status"] == "disabled"
        assert not (root / "maintenance").exists()
        results.append("disabled status/run-once are local and create no state")

        for print_mode, native in ((True, False), (False, False), (False, True)):
            item = fixture(f"print-{print_mode}-native-{native}", print_mode, native)
            root, snapshot, config, profile, _ = item
            before = {p: hashlib.sha256(p.read_bytes()).hexdigest() for p in [profile, *snapshot.iterdir()]}
            key = apply(item)
            report = call(root, "warmup", "--provider", "claude", "--account", key)
            assert report["results"][0]["status"] == "completed", report
            generations = [json.loads(line) for line in (config / "generations.jsonl").read_text().splitlines()]
            assert len(generations) == 1 and not generations[0]["forbidden"]
            assert all(hashlib.sha256(p.read_bytes()).hexdigest() == sha for p, sha in before.items())
            assert not call(root, "status", "--provider", "claude")["status"]["enabled"]
            results.append(f"explicit fake warmup: print={print_mode}, native_profile={native}")

        item = fixture("stale-plan")
        root, snapshot, config, profile, binding = item
        plan = call(root, "plan", "--provider", "claude", "--source", str(snapshot), "--file", str(binding))
        write(snapshot / "settings.json", {"claudeWarmup": "all"})
        call(root, "apply", "--provider", "claude", "--source", str(snapshot), "--file", str(binding), "--expect-plan", plan["plan_hash"], success=False)
        assert not (root / "maintenance/claude/state.json").exists() and not (config / "generations.jsonl").exists()
        results.append("stale plan rejects without native CLI or policy write")

        item = fixture("held", no_extra=True)
        root, _, config, _, _ = item
        key = apply(item)
        write(config / "fake.json", {"used": 100})
        report = call(root, "warmup", "--provider", "claude", "--account", key)
        assert report["results"][0]["status"] != "completed" and not (config / "generations.jsonl").exists(), report
        results.append("full quota plus no-extra sends no generation")

        item = fixture("unknown")
        root, _, config, _, _ = item
        key = apply(item)
        write(config / "fake.json", {"uncertain": True})
        first = call(root, "warmup", "--provider", "claude", "--account", key)
        second = call(root, "warmup", "--provider", "claude", "--account", key)
        assert first["results"][0]["status"] == "unknown" and second["results"][0]["status"] == "pending_review", (first, second)
        assert len((config / "generations.jsonl").read_text().splitlines()) == 1
        results.append("unknown send is durable and is never automatically replayed")

        call(root, "worker", "--provider", "claude", success=False)
        call(root, "warmup", "--account", key, success=False)
        results.append("Codex reset worker and manual provider boundary remain explicit")
    print(json.dumps({"passed": len(results), "cases": results, "real_accounts": 0, "network_requests": 0}, indent=2))


if __name__ == "__main__":
    main()
