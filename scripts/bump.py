#!/usr/bin/env python3
"""Change a semantic version; --release commits, tags, and pushes after local checks."""
import argparse
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parent.parent

def run(*args):
    return subprocess.check_output(args, cwd=ROOT, text=True).strip()

def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("part", help="patch, minor, major, or an explicit X.Y.Z")
    p.add_argument("--release", action="store_true")
    args = p.parse_args()
    current = (ROOT / "VERSION").read_text().strip()
    if not re.fullmatch(r"\d+\.\d+\.\d+", current):
        p.error("VERSION must contain X.Y.Z")
    a, b, c = map(int, current.split("."))
    choices = {"patch": f"{a}.{b}.{c+1}", "minor": f"{a}.{b+1}.0", "major": f"{a+1}.0.0"}
    value = choices.get(args.part, args.part)
    if not re.fullmatch(r"\d+\.\d+\.\d+", value) or value == current:
        p.error("new version must be a different X.Y.Z")
    if args.release and run("git", "status", "--porcelain"):
        p.error("release requires a clean worktree")
    (ROOT / "VERSION").write_text(value + "\n")
    print(f"{current} -> {value}")
    if args.release:
        subprocess.run(["make", "check"], cwd=ROOT, check=True)
        subprocess.run(["git", "add", "VERSION"], cwd=ROOT, check=True)
        subprocess.run(["git", "commit", "-m", f"Release v{value}"], cwd=ROOT, check=True)
        subprocess.run([sys.executable, "scripts/release.py", "tag"], cwd=ROOT, check=True)

if __name__ == "__main__":
    main()
