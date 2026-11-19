#!/usr/bin/env python3
"""Publish a generated and checksummed AUR recipe with a package-scoped SSH key."""
import argparse
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import tempfile

def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--dir", type=Path, required=True)
    args = p.parse_args()
    source = args.dir.resolve()
    pkg = (source / "PKGBUILD").read_text()
    info = (source / ".SRCINFO").read_text()
    if "pkgname=zcalld-compat\n" not in pkg or "pkgname = zcalld-compat" not in info:
        p.error("only the zcalld-compat recipe can be published")
    if not re.search(r"sha256sums = [0-9a-f]{64}\n", info):
        p.error("generated source checksum is required")
    key = os.environ.get("AUR_SSH_PRIVATE_KEY", "")
    known = os.environ.get("AUR_KNOWN_HOSTS", "")
    if not key or not known:
        p.error("AUR key and pinned known-hosts are required")
    with tempfile.TemporaryDirectory(prefix="zcalld-aur-") as tmp:
        root = Path(tmp)
        private = root / "identity"
        private.write_text(key.rstrip() + "\n")
        private.chmod(0o600)
        hosts = root / "known_hosts"
        hosts.write_text(known.rstrip() + "\n")
        hosts.chmod(0o600)
        env = os.environ.copy()
        env.pop("AUR_SSH_PRIVATE_KEY", None)
        env.pop("AUR_KNOWN_HOSTS", None)
        env["GIT_SSH_COMMAND"] = "ssh -i " + shlex.quote(str(private)) + " -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=" + shlex.quote(str(hosts))
        repo = root / "aur"
        subprocess.run(["git", "clone", "ssh://aur@aur.archlinux.org/zcalld-compat.git", str(repo)], env=env, check=True)
        for name in ("PKGBUILD", ".SRCINFO"):
            shutil.copyfile(source / name, repo / name)
        subprocess.run(["git", "diff", "--check"], cwd=repo, check=True)
        status = subprocess.check_output(["git", "status", "--porcelain"], cwd=repo, text=True).strip()
        if not status:
            print("AUR recipe already matches")
            return
        subprocess.run(["git", "config", "user.name", "LIghtJUNction"], cwd=repo, check=True)
        subprocess.run(["git", "config", "user.email", "lightjunction@users.noreply.github.com"], cwd=repo, check=True)
        subprocess.run(["git", "add", "PKGBUILD", ".SRCINFO"], cwd=repo, check=True)
        value = re.search(r"pkgver = ([0-9.]+)", info).group(1)
        subprocess.run(["git", "commit", "-m", "Release " + value], cwd=repo, check=True)
        subprocess.run(["git", "push", "origin", "HEAD:master"], cwd=repo, env=env, check=True)

if __name__ == "__main__":
    main()
