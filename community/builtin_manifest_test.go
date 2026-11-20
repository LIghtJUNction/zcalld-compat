package community

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
)

// Public, synthetic fixture from the exact nine embedded runtime files of
// release v0.1.6/e4dffde8. It contains no account state or private configuration.
//
//go:embed testdata/builtin-0.1.6.tar.gz
var legacyBuiltinArchive []byte

func legacyBuiltinFixture(t *testing.T, root string) (string, map[string]string) {
	t.Helper()
	raw, err := builtinManifests.ReadFile("builtin-manifests/v0.1.6.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest builtinManifest
	if json.Unmarshal(raw, &manifest) != nil || manifest.Release != "0.1.6" ||
		manifest.Commit != "e4dffde8012a9120df00d2b7b93e19933007a19f" {
		t.Fatal("invalid pinned release manifest")
	}
	dir := filepath.Join(root, "runtime", "builtin", manifest.SHA256[:24])
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for p := dir; p != filepath.Dir(root); p = filepath.Dir(p) {
		if err := privatefile.Protect(p, 0700); err != nil {
			t.Fatal(err)
		}
		if p == root {
			break
		}
	}
	gzipReader, err := gzip.NewReader(bytes.NewReader(legacyBuiltinArchive))
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	count := 0
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != header.Name || header.Size > 1<<20 {
			t.Fatal("invalid fixture member")
		}
		data, err := io.ReadAll(io.LimitReader(reader, header.Size+1))
		if err != nil || int64(len(data)) != header.Size {
			t.Fatal("invalid fixture body", err)
		}
		if err := Atomic(filepath.Join(dir, header.Name), data, 0600); err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != len(manifest.Files) {
		t.Fatal("fixture member count")
	}
	if err := verifyBuiltinDirectory(root, dir, manifest); err != nil {
		t.Fatal("official fixture identity", err)
	}
	sums := map[string]string{}
	for _, file := range manifest.Files {
		sums[file.Name] = file.SHA256
	}
	return dir, sums
}

func TestRuntimeRejectsUnverifiedOldAndDestinationBundles(t *testing.T) {
	cases := []string{"edited-byte", "extra-file", "wrong-directory", "wrong-entry-sha", "external-path", "unverified-destination"}
	if runtime.GOOS != "windows" {
		cases = append(cases, "symlink-file", "symlink-directory")
	}
	for _, change := range cases {
		t.Run(change, func(t *testing.T) {
			root := runtimeFixture(t)
			old, sums := legacyBuiltinFixture(t, root)
			path := filepath.Join(old, "codex.mjs")
			sum := sums["codex.mjs"]
			switch change {
			case "edited-byte":
				p := filepath.Join(old, "common.mjs")
				data, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				data[0] ^= 1
				if err := os.WriteFile(p, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "extra-file":
				if err := Atomic(filepath.Join(old, "unknown.mjs"), []byte("export default {}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong-directory":
				next := filepath.Join(filepath.Dir(old), strings.Repeat("a", 24))
				if err := os.Rename(old, next); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(next, "codex.mjs")
			case "wrong-entry-sha":
				sum = strings.Repeat("0", 64)
			case "external-path":
				path = filepath.Join(root, "custom-codex.mjs")
				data, err := os.ReadFile(filepath.Join(old, "codex.mjs"))
				if err != nil {
					t.Fatal(err)
				}
				if err := Atomic(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "unverified-destination":
				current, err := currentBuiltinManifest()
				if err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(root, "runtime", "builtin", current.SHA256[:24])
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
				if err := privatefile.Protect(target, 0700); err != nil {
					t.Fatal(err)
				}
				if err := Atomic(filepath.Join(target, "custom.mjs"), []byte("custom"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink-file":
				p := filepath.Join(old, "common.mjs")
				if err := os.Rename(p, p+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(p+".saved", p); err != nil {
					t.Fatal(err)
				}
			case "symlink-directory":
				if err := os.Rename(old, old+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(old+".saved", old); err != nil {
					t.Fatal(err)
				}
			}
			registry, _ := json.Marshal(map[string]any{"version": 1, "plugins": []map[string]any{{
				"name": "builtin-codex", "source": "builtin-codex", "kind": "community", "enabled": true, "path": path, "sha256": sum,
			}}, "accounts": map[string]any{"opaque-account": map[string]any{"enabled": true}}})
			auth := []byte("{ \"opaque-account\": {\"refreshOwner\":\"external\",\"access\":\"PRIVATE-UNCHANGED\"} }\n")
			binding := []byte("{\"unchanged\":true}\n")
			for name, data := range map[string][]byte{"registry.json": registry, "plugin-auth.json": auth, "host.json": binding} {
				if err := Atomic(filepath.Join(root, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := ConfigureRuntime(root); err == nil {
				t.Fatal("unverified builtin was adopted")
			}
			for name, want := range map[string][]byte{"registry.json": registry, "plugin-auth.json": auth, "host.json": binding} {
				got, err := os.ReadFile(filepath.Join(root, name))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatal("failed verification changed", name, err)
				}
			}
		})
	}
}

func TestRuntimeOldBuiltinCapabilityRequiresExplicitSafeRefresh(t *testing.T) {
	root := runtimeFixture(t)
	old, sums := legacyBuiltinFixture(t, root)
	r := Registry{Version: 1, Plugins: []Entry{{Name: "builtin-codex", Source: "builtin-codex", Kind: "community", Enabled: true,
		Path: filepath.Join(old, "codex.mjs"), SHA256: sums["codex.mjs"]}}}
	if _, reason := verifiedBuiltinCodex(root, r); reason != "builtin_runtime_outdated" {
		t.Fatal(reason)
	}
	if err := Save(root, r); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureRuntime(root); err != nil {
		t.Fatal(err)
	}
	updated, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := verifiedBuiltinCodex(root, updated); reason != "" {
		t.Fatal(reason)
	}
}
