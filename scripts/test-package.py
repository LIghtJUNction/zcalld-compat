#!/usr/bin/env python3
"""Inspect actual maintenance support archives without activation or network."""
import importlib.util
from pathlib import Path
import plistlib
import tarfile
import tempfile
import unittest
import xml.etree.ElementTree as ET
import zipfile

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("release", ROOT / "scripts/release.py")
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)

DOCS = (
    "docs/CODEX-MAINTENANCE.md",
    "docs/CLAUDE-MAINTENANCE.md",
    "docs/CLAUDE-TUI-SOURCE.json",
    "packaging/launchd/org.zcalld.maintenance.plist.in",
    "packaging/windows/zcalld-maintenance.xml.in",
)
UNITS = ("zcalld-maintenance.service", "zcalld-maintenance.timer")


class MaintenancePackageTests(unittest.TestCase):
    def test_linux_archive_installs_units_without_enabling_them(self):
        with tempfile.TemporaryDirectory() as temp:
            base = Path(temp)
            stage = base / "stage"
            release.stage_maintenance(stage / "usr", "linux")
            dest = base / "support.tar.gz"
            release.archive_tree(stage, dest, 1700000000)
            with tarfile.open(dest, "r:gz") as archive:
                files = {entry.name: entry for entry in archive.getmembers()}
                expected = {"usr/share/doc/zcalld-compat/" + name for name in DOCS}
                expected.update("usr/lib/systemd/user/" + name for name in UNITS)
                self.assertEqual(set(files), expected)
                for path, entry in files.items():
                    self.assertTrue(entry.isfile())
                    self.assertEqual(entry.mode, 0o644)
                    source = (ROOT / "packaging/systemd/user" / Path(path).name
                              if path.startswith("usr/lib/")
                              else ROOT / path.removeprefix("usr/share/doc/zcalld-compat/"))
                    self.assertEqual(archive.extractfile(entry).read(), source.read_bytes())
                self.assertFalse(any(".wants/" in path or path.startswith("etc/") for path in files))

    def test_other_archives_only_ship_inactive_user_templates(self):
        for goos in ("darwin", "windows"):
            with self.subTest(platform=goos), tempfile.TemporaryDirectory() as temp:
                base = Path(temp)
                stage = base / "stage"
                prefix = stage if goos == "windows" else stage / "usr"
                release.stage_maintenance(prefix, goos)
                suffix = ".zip" if goos == "windows" else ".tar.gz"
                dest = base / ("support" + suffix)
                release.archive_tree(stage, dest, 1700000000)
                lead = "" if goos == "windows" else "usr/"
                expected = {lead + "share/doc/zcalld-compat/" + name for name in DOCS}
                if goos == "windows":
                    with zipfile.ZipFile(dest) as archive:
                        self.assertEqual(set(archive.namelist()), expected)
                        contents = {name: archive.read(name) for name in expected}
                        for name in expected:
                            self.assertEqual(archive.getinfo(name).external_attr >> 16 & 0o777, 0o644)
                else:
                    with tarfile.open(dest, "r:gz") as archive:
                        self.assertEqual(set(archive.getnames()), expected)
                        contents = {name: archive.extractfile(name).read() for name in expected}
                doc = lead + "share/doc/zcalld-compat/"
                launchd = plistlib.loads(contents[doc + "packaging/launchd/org.zcalld.maintenance.plist.in"])
                self.assertFalse(launchd["RunAtLoad"])
                self.assertEqual(launchd["ProgramArguments"], ["@ZCALLD_BIN@", "plugins", "maintenance", "run-once"])
                task = ET.fromstring(contents[doc + "packaging/windows/zcalld-maintenance.xml.in"])
                ns = {"t": "http://schemas.microsoft.com/windows/2004/02/mit/task"}
                self.assertEqual(task.find("t:Settings/t:Enabled", ns).text, "false")
                self.assertEqual(task.find("t:Settings/t:MultipleInstancesPolicy", ns).text, "IgnoreNew")
                self.assertEqual(task.find("t:Settings/t:ExecutionTimeLimit", ns).text, "PT1M15S")


if __name__ == "__main__":
    unittest.main()
