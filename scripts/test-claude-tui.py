#!/usr/bin/env python3
"""Exercise the built PTY/ConPTY helper with the compiled upstream fake CLI.

Build (native, never cross-compile and call it execution):
  go build -o /TEMP/zcalld-claude-tui[.exe] ./cmd/zcalld-claude-tui
  go test -c -o /TEMP/fake-claude[.exe] ./internal/claudetui
Run this file with --helper and --fake-claude pointing to those two files.
No actual Claude executable, account, authentication, or network is used.
"""

import argparse
import ctypes
import json
import os
from pathlib import Path
import queue
import signal
import subprocess
import tempfile
import threading
import time
import unittest


REVISION = "1e174f8f9f613a920203a4658d259456fcab5d0d"
HELPER = None
FAKE = None


class OwnedChild:
    def __init__(self, pid):
        self.pid = pid
        self.handle = None
        if os.name == "nt":
            self.kernel = ctypes.WinDLL("kernel32", use_last_error=True)
            self.kernel.OpenProcess.argtypes = [ctypes.c_ulong, ctypes.c_int, ctypes.c_ulong]
            self.kernel.OpenProcess.restype = ctypes.c_void_p
            self.kernel.WaitForSingleObject.argtypes = [ctypes.c_void_p, ctypes.c_ulong]
            self.kernel.WaitForSingleObject.restype = ctypes.c_ulong
            self.kernel.CloseHandle.argtypes = [ctypes.c_void_p]
            self.kernel.CloseHandle.restype = ctypes.c_int
            self.handle = self.kernel.OpenProcess(0x00100000, 0, pid)
            if not self.handle:
                raise AssertionError("cannot retain the synthetic child process identity")

    def wait_exited(self, timeout=5):
        if self.handle:
            if self.kernel.WaitForSingleObject(self.handle, int(timeout * 1000)) != 0:
                raise AssertionError("the owned fake Claude process did not exit")
            return
        end = time.monotonic() + timeout
        while True:
            try:
                os.kill(self.pid, 0)
            except ProcessLookupError:
                return
            if time.monotonic() >= end:
                break
            time.sleep(0.02)
        raise AssertionError("the owned fake Claude process did not exit")

    def close(self):
        if self.handle:
            self.kernel.CloseHandle(self.handle)
            self.handle = None


class NativeClaudeTUI(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="zcalld-claude-native-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.home = self.root / "home"
        self.home.mkdir(mode=0o700)
        self.work = self.root / "work"
        self.work.mkdir(mode=0o700)
        self.config = self.home / ".claude"
        self.config.mkdir(mode=0o700)
        self.profile = self.home / ".claude.json"
        self.profile.write_text("{}\n", encoding="utf-8")
        self.profile.chmod(0o600)
        self.processes = []
        self.children = []
        self.env = {
            k: v for k, v in os.environ.items()
            if not k.startswith(("ANTHROPIC_", "CLAUDE_", "MAGPIE_", "FAKE_", "ZCALLD_TUI_"))
            and k not in {"BUN_OPTIONS", "NODE_OPTIONS"}
        }
        self.env.update({
            "HOME": str(self.home), "USERPROFILE": str(self.home),
            "MAGPIE_TUI_TEST": "claude", "ZCALLD_TUI_NATIVE_PROBE": "1",
        })
        for key in ("TMPDIR", "TMP", "TEMP", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "APPDATA", "LOCALAPPDATA"):
            directory = self.root / key.lower()
            directory.mkdir(mode=0o700)
            self.env[key] = str(directory)
        self.addCleanup(self.cleanup_owned)

    def cleanup_owned(self):
        for proc in self.processes:
            if proc.poll() is None:
                if proc.stdin and not proc.stdin.closed:
                    proc.stdin.close()
                try:
                    proc.wait(timeout=8)
                except subprocess.TimeoutExpired:
                    proc.terminate()
                    try:
                        proc.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        proc.kill()
                        proc.wait(timeout=5)
            for pipe in (proc.stdin, proc.stdout, proc.stderr):
                if pipe and not pipe.closed:
                    pipe.close()
        for child in self.children:
            child.close()

    def args(self, seconds=20):
        return [
            str(HELPER), "--claude-binary", str(FAKE), "--config-dir", str(self.config),
            "--profile", str(self.profile), "--work-dir", str(self.work),
            "--deadline-ms", str(int((time.time() + seconds) * 1000)), "--",
            "-p", "--output-format", "stream-json", "--input-format", "stream-json",
            "--verbose", "--model", "haiku", "--tools", "", "--strict-mcp-config",
            "--setting-sources", "",
        ]

    def start(self, seconds=20, extra_env=None):
        env = self.env.copy()
        env.update(extra_env or {})
        proc = subprocess.Popen(self.args(seconds), stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, cwd=self.work, env=env)
        self.processes.append(proc)
        messages = queue.Queue()

        def read():
            for line in iter(proc.stdout.readline, b""):
                try:
                    messages.put(json.loads(line))
                except ValueError:
                    messages.put({"invalid_json": True})
            messages.put(None)

        threading.Thread(target=read, daemon=True).start()
        return proc, messages

    def until_message(self, proc, messages, kind, seconds=15):
        end = time.monotonic() + seconds
        seen = []
        while time.monotonic() < end:
            try:
                item = messages.get(timeout=max(0.01, end - time.monotonic()))
            except queue.Empty:
                break
            self.assertIsNotNone(item, "helper ended before the expected stream-json message")
            self.assertNotIn("invalid_json", item)
            seen.append(item)
            if item.get("type") == kind:
                return item, seen
        self.fail("timed out waiting for synthetic stream-json " + kind)

    def launch(self, seconds=10):
        path = self.config / "native-launch.json"
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if path.is_file():
                try:
                    value = json.loads(path.read_text(encoding="utf-8"))
                except ValueError:
                    time.sleep(0.01)
                    continue
                child = OwnedChild(value["pid"])
                self.children.append(child)
                return value, child
            time.sleep(0.01)
        self.fail("the synthetic Claude process did not report its launch")

    def send_hi(self, proc):
        proc.stdin.write(b'{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}\n')
        proc.stdin.flush()

    def finish(self, proc, child):
        proc.stdin.close()
        self.assertEqual(proc.wait(timeout=10), 0, proc.stderr.read().decode("utf-8", "replace"))
        child.wait_exited()

    def assert_interactive_args(self, value):
        for key in ("-p", "--print", "--output-format", "--input-format", "--verbose", "--dangerously-skip-permissions"):
            self.assertNotIn(key, value["args"])
        self.assertIn("--session-id", value["args"])
        self.assertIn("--permission-mode", value["args"])
        self.assertIn("--no-chrome", value["args"])
        self.assertEqual(value["bypass"], "")
        self.assertEqual(value["guard"], "")
        self.assertEqual(value["helper_config"], "")

    def test_capabilities_launch_no_cli(self):
        before = sorted(str(x.relative_to(self.root)) for x in self.root.rglob("*"))
        got = subprocess.run([str(HELPER), "--capabilities"], env=self.env, cwd=self.work,
                             capture_output=True, timeout=5, check=True)
        cap = json.loads(got.stdout)
        self.assertEqual(cap["schema_version"], 1)
        self.assertEqual(cap["source_revision"], REVISION)
        for key in ("interactive", "stream_json", "claude_binary_absolute", "config_dir_explicit", "profile_path_explicit"):
            self.assertIs(cap[key], True)
        self.assertIs(cap["print_fallback"], False)
        self.assertIs(cap["wsl"], False)
        self.assertEqual(before, sorted(str(x.relative_to(self.root)) for x in self.root.rglob("*")))

    def test_native_default_profile_streams_hi(self):
        proc, messages = self.start(extra_env={"MAGPIE_CLAUDE_TUI_BYPASS": "1", "MAGPIE_CLAUDE_GUARD": "must-not-reach-child"})
        self.send_hi(proc)
        result, seen = self.until_message(proc, messages, "result")
        self.assertIs(result.get("is_error"), False)
        self.assertTrue(any("echo: hi" in json.dumps(x) for x in seen))
        launch, child = self.launch()
        self.assert_interactive_args(launch)
        self.assertEqual(launch["config_env"], "")
        self.assertEqual(Path(launch["profile"]), self.profile)
        self.assertEqual(Path(launch["config"]), self.config)
        self.finish(proc, child)

    def test_explicit_config_profile_streams_hi(self):
        self.config = self.root / "isolated-account"
        self.config.mkdir(mode=0o700)
        self.profile = self.config / ".claude.json"
        self.profile.write_text("{}\n", encoding="utf-8")
        proc, messages = self.start()
        self.send_hi(proc)
        result, seen = self.until_message(proc, messages, "result")
        self.assertIs(result.get("is_error"), False)
        self.assertTrue(any("echo: hi" in json.dumps(x) for x in seen))
        launch, child = self.launch()
        self.assert_interactive_args(launch)
        self.assertEqual(Path(launch["config_env"]), self.config)
        self.assertEqual(Path(launch["profile"]), self.profile)
        self.finish(proc, child)

    def wait_turn(self, seconds=12):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if (self.config / "turn.started").is_file():
                return
            time.sleep(0.01)
        self.fail("the synthetic turn did not reach its transcript")

    def test_eof_cancels_owned_inflight_cli(self):
        proc, messages = self.start(extra_env={"ZCALLD_TUI_NATIVE_HOLD": "1"})
        self.send_hi(proc)
        self.until_message(proc, messages, "system")
        _, child = self.launch()
        self.wait_turn()
        self.finish(proc, child)

    def test_deadline_cancels_owned_inflight_cli(self):
        proc, messages = self.start(seconds=5, extra_env={"ZCALLD_TUI_NATIVE_HOLD": "1"})
        self.send_hi(proc)
        self.until_message(proc, messages, "system", seconds=4)
        _, child = self.launch()
        self.wait_turn(seconds=4)
        self.assertNotEqual(proc.wait(timeout=12), 0)
        child.wait_exited()

    def test_eof_waits_for_forced_child_reap(self):
        proc, messages = self.start(extra_env={
            "ZCALLD_TUI_NATIVE_HOLD": "1", "ZCALLD_TUI_NATIVE_IGNORE_STOP": "1",
        })
        self.send_hi(proc)
        self.until_message(proc, messages, "system")
        _, child = self.launch()
        self.wait_turn()
        started = time.monotonic()
        proc.stdin.close()
        self.assertEqual(proc.wait(timeout=5), 0, proc.stderr.read().decode("utf-8", "replace"))
        # No extra grace after the helper has reported success: the owned
        # process must already be signaled/reaped on all native platforms.
        child.wait_exited(timeout=0)
        self.assertLess(time.monotonic() - started, 4)

    def test_helper_deadline_leaves_original_budget_for_drain(self):
        caller_deadline = time.monotonic() + 13
        proc, messages = self.start(seconds=5, extra_env={
            "ZCALLD_TUI_NATIVE_HOLD": "1", "ZCALLD_TUI_NATIVE_IGNORE_STOP": "1",
        })
        self.send_hi(proc)
        self.until_message(proc, messages, "system", seconds=4)
        _, child = self.launch()
        self.wait_turn(seconds=4)
        self.assertNotEqual(proc.wait(timeout=max(0.1, caller_deadline - 4 - time.monotonic())), 0)
        child.wait_exited(timeout=0)
        self.assertLess(time.monotonic(), caller_deadline - 4)

    def test_signin_failure_never_switches_to_print(self):
        proc, messages = self.start(extra_env={"FAKE_LOGIN": "1"})
        self.send_hi(proc)
        _, child = self.launch()
        self.assertNotEqual(proc.wait(timeout=10), 0)
        child.wait_exited()
        launch = json.loads((self.config / "native-launch.json").read_text(encoding="utf-8"))
        self.assert_interactive_args(launch)
        self.assertIn("sign", proc.stderr.read().decode("utf-8", "replace").lower())
        while not messages.empty():
            self.assertNotIn("print mode", json.dumps(messages.get_nowait()))

    def test_unsupported_profile_layout_never_starts_cli(self):
        self.profile = self.root / "unrelated-profile.json"
        self.profile.write_text("{}\n", encoding="utf-8")
        result = subprocess.run(self.args(), env=self.env, cwd=self.work, capture_output=True, timeout=5)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b"not a supported native Claude layout", result.stderr)
        self.assertFalse((self.config / "native-launch.json").exists())


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--helper", type=Path, required=True)
    parser.add_argument("--fake-claude", type=Path, required=True)
    ns, rest = parser.parse_known_args()
    HELPER, FAKE = ns.helper.resolve(strict=True), ns.fake_claude.resolve(strict=True)
    unittest.main(argv=[__file__, *rest])
