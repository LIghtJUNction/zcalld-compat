#!/usr/bin/env python3
"""Execute the release archive's helpers on their matching native runner."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import subprocess
import sys
import tarfile
import tempfile
import zipfile


ROOT = Path(__file__).resolve().parent.parent


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--target", required=True)
    args = parser.parse_args()
    machine = platform.machine().lower()
    architecture = "aarch64" if machine in {"arm64", "aarch64"} else "x86_64" if machine in {"amd64", "x86_64"} else "unsupported"
    operating = {"Linux": "linux", "Darwin": "macos", "Windows": "windows"}.get(platform.system(), "unsupported")
    actual = operating + "-" + architecture
    if actual != args.target:
        raise SystemExit(f"native execution requires {args.target}, running on {actual}")
    value = (ROOT / "VERSION").read_text().strip()
    suffix = ".zip" if os.name == "nt" else ".tar.gz"
    archive = ROOT / "dist" / f"zcalld-compat-{value}-{args.target}{suffix}"
    if not archive.is_file():
        raise SystemExit("the official build step must produce this exact target archive first")
    prefix = "" if os.name == "nt" else "usr/"
    executable_suffix = ".exe" if os.name == "nt" else ""
    wanted = {name: prefix + "libexec/zcalld/" + name + executable_suffix
              for name in ("zcalld-privacy", "zcalld-pluginctl", "zcalld-claude-tui")}
    licenses = {
        prefix + "share/licenses/zcalld-compat/LICENSE.magpie": ROOT / "LICENSE.magpie",
        prefix + "share/licenses/zcalld-compat/third-party/golang.org__x__text@v0.39.0.txt": ROOT / "LICENSES/golang.org__x__text@v0.39.0.txt",
        prefix + "share/doc/zcalld-compat/NOTICE.magpie": ROOT / "NOTICE.magpie",
    }
    env = os.environ.copy()
    env.update(CGO_ENABLED="0")
    for key in ("GOOS", "GOARCH"):
        env.pop(key, None)

    def run(*argv, timeout=120):
        subprocess.run(list(map(str, argv)), cwd=ROOT, env=env, check=True, timeout=timeout)

    with tempfile.TemporaryDirectory(prefix="zcalld-native-claude-") as temporary:
        temp = Path(temporary).resolve()
        if suffix == ".zip":
            container = zipfile.ZipFile(archive)
            names = container.namelist()
            def read(name):
                entry = container.getinfo(name)
                if entry.is_dir() or entry.file_size > 64 << 20 or (entry.external_attr >> 16) & 0o170000 == 0o120000:
                    raise AssertionError("unsafe native package member")
                return container.read(name)
        else:
            container = tarfile.open(archive, "r:gz")
            names = container.getnames()
            def read(name):
                entry = container.getmember(name)
                if not entry.isfile() or entry.size > 64 << 20:
                    raise AssertionError("unsafe native package member")
                return container.extractfile(entry).read()
        binaries = {}
        with container:
            assert len(names) == len(set(names)), "duplicate archive member"
            for name, source in licenses.items():
                assert read(name) == source.read_bytes(), "missing or altered license: " + name
            for name, member in wanted.items():
                path = temp / (name + executable_suffix)
                path.write_bytes(read(member))
                path.chmod(0o755)
                observed = subprocess.check_output([str(path), "--version"], text=True, env=env, timeout=10).strip()
                assert observed.startswith(name + " " + value + " ("), observed
                binaries[name] = path
        fake_tui = temp / ("fake-claude-tui" + executable_suffix)
        fake_maintenance = temp / ("fake-claude-maintenance" + executable_suffix)
        run("go", "test", "-mod=readonly", "-c", "-o", fake_tui, "./internal/claudetui")
        run("go", "test", "-mod=readonly", "-c", "-o", fake_maintenance, "./claudemaintenance")
        run("go", "test", "-mod=readonly", "-count=1", "-run", "^TestProcess", "./claudemaintenance")
        run(sys.executable, "scripts/test-claude-tui.py", "--helper", binaries["zcalld-claude-tui"], "--fake-claude", fake_tui)
        run(sys.executable, "scripts/test-claude-maintenance.py", "--helper", binaries["zcalld-pluginctl"], "--fake-claude", fake_maintenance)
        print(json.dumps({"target": actual, "source_version": value, "archive": archive.name,
                          "archive_sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
                          "helpers": {name: hashlib.sha256(path.read_bytes()).hexdigest() for name, path in binaries.items()},
                          "licenses_verified": sorted(licenses), "real_accounts": 0, "real_provider_requests": 0}, indent=2))


if __name__ == "__main__":
    main()
