# Arch package revisions

The Arch package keeps helper executables and their runtime resources in
`/usr/lib/zcalld-compat/`. The commands `zcalld-pluginctl` and
`zcalld-privacy` are relative symlinks from `/usr/bin/`. Core 0.2.3 or newer
is required because older core releases do not discover this directory.
The `zcalld-bin` package supplies a versioned `zcalld` capability.

Arch builds enable CGO, PIE and the external linker, and pass the distribution
CPPFLAGS, CFLAGS, CXXFLAGS and LDFLAGS through the corresponding CGO variables.
The portable release artifacts retain their separate cross-platform build
settings. See the official
[Arch package guidelines](https://wiki.archlinux.org/title/Arch_package_guidelines)
and [Go package guidelines](https://wiki.archlinux.org/title/Go_package_guidelines).

## Revise a recipe without replacing a release

Read the actual GitHub release asset metadata, download its source archive and
SHA256SUMS, and verify the source checksum before rendering. A package revision
uses the existing immutable source archive. It does not recreate the runtime
release or move its tag.

    python3 scripts/release.py aur \
      --source /absolute/zcalld-compat-0.1.5-source.tar.gz \
      --version 0.1.5 \
      --sha256 VERIFIED_SOURCE_SHA256 \
      --pkgrel 2 \
      --out /absolute/aur-revision

The renderer checks the supplied checksum and the archive's VERSION entry.
The ordinary full release stage uses pkgrel 1 for each new upstream version.
A revision of an already published upstream version must increase pkgrel.

With the declared dependencies installed, inspect the rendered recipe and run
`makepkg --printsrcinfo`, `makepkg --verifysource`, and
`makepkg --check --cleanbuild --log` from its directory. Compare generated
.SRCINFO, inspect the package layout and permissions, and run namcap against
both PKGBUILD and the package. Do not bypass dependency or check requirements.

After independent review, publish only the verified recipe:

    python3 scripts/publish-aur.py --dir /absolute/aur-revision

The publisher uses the existing AUR credential environment and pinned host
keys. It rejects lower versions or package revisions. An unchanged recipe at
the same version/revision is a no-op; changed contents require a new revision.
Its focused offline regression tests run with `python3 scripts/test-aur.py`
and are included in `make check`.

Publish a revision that introduces a higher core requirement only after the
corresponding core AUR package is available. Do not rerun an old full release
workflow to publish a recipe revision: historical tags retain their historical
publisher code. Use the current guarded publisher instead.
