#!/usr/bin/env python3
"""Build portable helpers, stage checksummed release assets, or publish a tag."""
import argparse
from datetime import datetime, timezone
import gzip
import hashlib
import json
import os
import platform
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile
import zipfile

ROOT = Path(__file__).resolve().parent.parent
TARGETS = {
    "linux-x86_64": ("linux", "amd64"),
    "linux-aarch64": ("linux", "arm64"),
    "windows-x86_64": ("windows", "amd64"),
    "macos-aarch64": ("darwin", "arm64"),
    "macos-x86_64": ("darwin", "amd64"),
}
PACKAGE = "github.com/LIghtJUNction/zcalld-compat/internal/buildinfo"

def run(*args, **kwargs):
    return subprocess.check_output(args, cwd=ROOT, **kwargs)

def version():
    value = (ROOT / "VERSION").read_text().strip()
    if not re.fullmatch(r"\d+\.\d+\.\d+", value):
        raise SystemExit("VERSION must contain X.Y.Z")
    return value

def sha(path):
    with path.open("rb") as f:
        return hashlib.file_digest(f, "sha256").hexdigest()

def epoch():
    return int(run("git", "show", "-s", "--format=%ct", "HEAD", text=True).strip())

def archive_tree(source, dest, timestamp):
    if dest.suffix == ".zip":
        when = datetime.fromtimestamp(max(timestamp, 315532800), timezone.utc).timetuple()[:6]
        with zipfile.ZipFile(dest, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as z:
            for file in sorted(source.rglob("*")):
                if file.is_file():
                    info = zipfile.ZipInfo(file.relative_to(source).as_posix(), when)
                    info.compress_type = zipfile.ZIP_DEFLATED
                    info.external_attr = (0o100000 | (file.stat().st_mode & 0o777)) << 16
                    z.writestr(info, file.read_bytes())
        return
    with dest.open("wb") as raw, gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=timestamp) as gz:
        with tarfile.open(fileobj=gz, mode="w") as tar:
            for file in sorted(source.rglob("*")):
                if file.is_file():
                    info = tar.gettarinfo(str(file), file.relative_to(source).as_posix())
                    info.uid = info.gid = 0
                    info.uname = info.gname = "root"
                    info.mtime = timestamp
                    with file.open("rb") as f:
                        tar.addfile(info, f)

def stage_maintenance(prefix, goos):
    """Package documentation and inactive scheduler templates, without activation."""
    if goos not in {"linux", "darwin", "windows"}:
        raise ValueError("unsupported maintenance scheduler platform")
    doc = prefix / "share/doc/zcalld-compat"
    for name in (
        "docs/CODEX-MAINTENANCE.md",
        "packaging/launchd/org.zcalld.maintenance.plist.in",
        "packaging/windows/zcalld-maintenance.xml.in",
    ):
        dest = doc / name
        dest.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(ROOT / name, dest)
        dest.chmod(0o644)
    if goos == "linux":
        for name in ("zcalld-maintenance.service", "zcalld-maintenance.timer"):
            dest = prefix / "lib/systemd/user" / name
            dest.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / "packaging/systemd/user" / name, dest)
            dest.chmod(0o644)

def build(target, out):
    goos, goarch = TARGETS[target]
    value = version()
    revision = run("git", "rev-parse", "HEAD", text=True).strip()
    env = os.environ.copy()
    env.update(GOOS=goos, GOARCH=goarch, CGO_ENABLED="0", GOAMD64="v1")
    flags = f"-s -w -buildid= -X {PACKAGE}.Version={value} -X {PACKAGE}.Revision={revision}"
    out.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".stage-", dir=out) as tmp:
        stage = Path(tmp)
        prefix = stage if goos == "windows" else stage / "usr"
        libexec = prefix / "libexec/zcalld"
        libexec.mkdir(parents=True)
        for name in ("zcalld-privacy", "zcalld-pluginctl"):
            binary = libexec / (name + (".exe" if goos == "windows" else ""))
            subprocess.run(["go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false",
                            "-ldflags", flags, "-o", str(binary), "./cmd/" + name], cwd=ROOT, env=env, check=True)
            binary.chmod(0o755)
            if goos == "linux" and goarch == "amd64" and platform.system() == "Linux" and platform.machine() == "x86_64":
                subprocess.run([str(binary), "--version"], check=True)
        for name in ("host.js", "pi.js", "mcp-tools.mjs"):
            shutil.copyfile(ROOT / "community/vendor" / name, libexec / name)
        builtin = ROOT / "community/builtin"
        if builtin.exists():
            (libexec / "builtin").mkdir()
            for file in sorted(builtin.glob("*.mjs")) + list(builtin.glob("catalog.json")):
                shutil.copyfile(file, libexec / "builtin" / file.name)
        doc = prefix / "share/doc/zcalld-compat"
        doc.mkdir(parents=True)
        for name in ("README.md", "NOTICE", "NOTICE_ROUTING.md", "NOTICE_MCP.md", "UPSTREAM.json"):
            shutil.copyfile(ROOT / name, doc / name)
        stage_maintenance(prefix, goos)
        license_dir = prefix / "share/licenses/zcalld-compat"
        license_dir.mkdir(parents=True)
        shutil.copyfile(ROOT / "LICENSE", license_dir / "LICENSE")
        shutil.copytree(ROOT / "LICENSES", license_dir / "third-party")
        (doc / "BUILD.json").write_text(json.dumps({
            "version": value, "revision": revision, "target": target,
            "go": run("go", "version", text=True).strip(), "cgo": False,
            "model_weights_included": False,
        }, indent=2) + "\n")
        suffix = ".zip" if goos == "windows" else ".tar.gz"
        dest = out / f"zcalld-compat-{value}-{target}{suffix}"
        archive_tree(stage, dest, epoch())
        print(json.dumps({"artifact": str(dest), "bytes": dest.stat().st_size, "sha256": sha(dest)}))


def render_aur(out, value, checksum, pkgrel=1):
    if not re.fullmatch(r"\d+\.\d+\.\d+", value):
        raise SystemExit("AUR version must contain X.Y.Z")
    if not isinstance(pkgrel, int) or pkgrel < 1:
        raise SystemExit("pkgrel must be a positive integer")
    if not re.fullmatch(r"[0-9a-f]{64}", checksum):
        raise SystemExit("a SHA256 source checksum is required")
    rendered = {}
    for name in ("PKGBUILD", ".SRCINFO"):
        template = (ROOT / "packaging/aur" / (name + ".in")).read_text()
        rendered[name] = template.replace("@VERSION@", value).replace("@PKGREL@", str(pkgrel)).replace("@SHA256@", checksum)
    out.mkdir(parents=True, exist_ok=True)
    for name, text in rendered.items():
        (out / name).write_text(text)

def render_published_aur(source, out, value, checksum, pkgrel=1):
    if sha(source) != checksum:
        raise SystemExit("published source archive checksum mismatch")
    with tarfile.open(source, "r:gz") as archive:
        try:
            entry = archive.getmember(f"zcalld-compat-{value}/VERSION")
        except KeyError:
            raise SystemExit("source archive does not contain the requested version") from None
        if not entry.isfile() or entry.size > 64:
            raise SystemExit("invalid source VERSION entry")
        stream = archive.extractfile(entry)
        if stream is None or stream.read(65).decode("ascii").strip() != value:
            raise SystemExit("source archive version mismatch")
    render_aur(out, value, checksum, pkgrel)
    print(json.dumps({"version": value, "pkgrel": pkgrel, "sha256": checksum, "aur": str(out)}))

def stage_release(out):
    if run("git", "status", "--porcelain", text=True).strip():
        raise SystemExit("stage a release from committed sources")
    value = version()
    expected = [f"zcalld-compat-{value}-{t}" + (".zip" if t.startswith("windows") else ".tar.gz") for t in TARGETS]
    missing = [name for name in expected if not (out / name).is_file()]
    if missing:
        raise SystemExit("missing release assets: " + ", ".join(missing))
    source = out / f"zcalld-compat-{value}-source.tar.gz"
    raw = run("git", "archive", "--format=tar", "--prefix=zcalld-compat-" + value + "/", "HEAD")
    with source.open("wb") as f, gzip.GzipFile(filename="", mode="wb", fileobj=f, mtime=epoch()) as gz:
        gz.write(raw)
    aur = out / "aur"
    render_aur(aur, value, sha(source))
    aur_archive = out / f"zcalld-compat-{value}-aur.tar.gz"
    archive_tree(aur, aur_archive, epoch())
    assets = sorted([out / name for name in expected] + [source, aur_archive])
    (out / "SHA256SUMS").write_text("".join(f"{sha(p)}  {p.name}\n" for p in assets))
    print(json.dumps({"version": value, "assets": len(assets), "checksums": str(out / "SHA256SUMS")}))

def tag():
    value = version()
    if run("git", "status", "--porcelain", text=True).strip():
        raise SystemExit("release requires a clean worktree")
    subprocess.run(["git", "tag", "-a", "v" + value, "-m", "zcalld-compat " + value], cwd=ROOT, check=True)
    subprocess.run(["git", "push", "--atomic", "origin", "HEAD", "v" + value], cwd=ROOT, check=True)
    if os.environ.get("GITHUB_ACTIONS") == "true":
        subprocess.run(["gh", "workflow", "run", "release.yml", "--ref", "v" + value, "-f", "tag=v" + value], cwd=ROOT, check=True)

def main():
    p = argparse.ArgumentParser(description=__doc__)
    sub = p.add_subparsers(dest="command", required=True)
    b = sub.add_parser("build")
    b.add_argument("--target", choices=list(TARGETS) + ["all"], required=True)
    b.add_argument("--out", type=Path, default=ROOT / "dist")
    s = sub.add_parser("stage")
    s.add_argument("--out", type=Path, default=ROOT / "dist")
    a = sub.add_parser("aur", help="render an AUR revision from an already published source archive")
    a.add_argument("--source", type=Path, required=True)
    a.add_argument("--version", required=True)
    a.add_argument("--sha256", required=True)
    a.add_argument("--pkgrel", type=int, default=1)
    a.add_argument("--out", type=Path, required=True)
    sub.add_parser("tag")
    args = p.parse_args()
    if args.command == "build":
        for target in TARGETS if args.target == "all" else [args.target]:
            build(target, args.out.resolve())
    elif args.command == "stage":
        stage_release(args.out.resolve())
    elif args.command == "aur":
        render_published_aur(args.source.resolve(), args.out.resolve(), args.version, args.sha256, args.pkgrel)
    else:
        tag()

if __name__ == "__main__":
    main()
