#!/usr/bin/env python3
"""Exercise immutable source checks and monotonic AUR recipe revisions offline."""
import contextlib
import hashlib
import importlib.util
import io
from pathlib import Path
import re
import shlex
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent

def module(name, filename):
    spec = importlib.util.spec_from_file_location(name, ROOT / "scripts" / filename)
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value

release = module("release", "release.py")
publisher = module("publish_aur", "publish-aur.py")

def info(version="0.1.5", pkgrel=1):
    return f"pkgbase = zcalld-compat\n\tpkgver = {version}\n\tpkgrel = {pkgrel}\n"

class AURRevisionTests(unittest.TestCase):
    def test_generated_recipes_have_matching_dependencies(self):
        with tempfile.TemporaryDirectory() as temp:
            out = Path(temp)
            release.render_aur(out, release.version(), "a" * 64)
            recipe = (out / "PKGBUILD").read_text()
            match = re.search(r"^depends=\(([^)]*)\)$", recipe, re.MULTILINE)
            self.assertIsNotNone(match)
            recipe_dependencies = shlex.split(match.group(1))
            index_dependencies = [
                line.split("=", 1)[1].strip()
                for line in (out / ".SRCINFO").read_text().splitlines()
                if line.strip().startswith("depends =")
            ]
            self.assertIn("zcalld>=0.4.0", recipe_dependencies)
            self.assertCountEqual(index_dependencies, recipe_dependencies)

    def test_revisions_cannot_downgrade_or_mutate_same_revision(self):
        with self.assertRaises(SystemExit):
            publisher.require_forward_revision(info(pkgrel=1), info(pkgrel=2), False)
        with self.assertRaises(SystemExit):
            publisher.require_forward_revision(info("0.1.4", 9), info("0.1.5", 2), False)
        with self.assertRaises(SystemExit):
            publisher.require_forward_revision(info(pkgrel=2), info(pkgrel=2), False)

    def test_noop_and_forward_revisions_are_allowed(self):
        publisher.require_forward_revision(info(pkgrel=2), info(pkgrel=2), True)
        publisher.require_forward_revision(info(pkgrel=2), info(pkgrel=1), False)
        publisher.require_forward_revision(info("0.1.6", 1), info("0.1.5", 2), False)

    def test_ambiguous_metadata_is_refused(self):
        for invalid in [info() + "\tpkgrel = 2\n", info(pkgrel=0), info() + "\tepoch = 1\n"]:
            with self.subTest(invalid=invalid), self.assertRaises(SystemExit):
                publisher.recipe_revision(invalid)

    def test_published_source_hash_and_version_are_required(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source = root / "source.tar.gz"
            with tarfile.open(source, "w:gz") as tar:
                data = b"0.1.5\n"
                entry = tarfile.TarInfo("zcalld-compat-0.1.5/VERSION")
                entry.size = len(data)
                tar.addfile(entry, io.BytesIO(data))
            checksum = hashlib.sha256(source.read_bytes()).hexdigest()
            for version, digest, revision in [("0.1.5", "0" * 64, 2), ("0.1.6", checksum, 2), ("0.1.5", checksum, 0)]:
                with self.subTest(version=version, digest=digest, revision=revision), self.assertRaises(SystemExit):
                    release.render_published_aur(source, root / "rejected", version, digest, revision)
                self.assertFalse((root / "rejected").exists())
            with contextlib.redirect_stdout(io.StringIO()):
                release.render_published_aur(source, root / "aur", "0.1.5", checksum, 2)
            text = (root / "aur" / "PKGBUILD").read_text()
            self.assertIn("pkgver=0.1.5\npkgrel=2\n", text)
            self.assertIn(checksum, text)
            release.render_aur(root / "next", "0.1.6", checksum)
            self.assertIn("pkgrel=1\n", (root / "next" / "PKGBUILD").read_text())

if __name__ == "__main__":
    unittest.main()
