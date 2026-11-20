package community

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
)

//go:embed builtin-manifests/*.json
var builtinManifests embed.FS

var errBuiltinUnverified = errors.New("registered builtin runtime does not match a trusted bundle; registry and credentials were not changed")
var errBuiltinOutdated = errors.New("verified builtin runtime is outdated; run zcalld plugins runtime configure for this configuration")

type builtinManifestFile struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type builtinManifest struct {
	Version int                   `json:"version"`
	Release string                `json:"release"`
	Commit  string                `json:"commit"`
	SHA256  string                `json:"bundle_sha256"`
	Files   []builtinManifestFile `json:"files"`
}

func currentBuiltinManifest() (builtinManifest, error) {
	result := builtinManifest{Version: 1, Release: "embedded"}
	entries, err := builtinFiles.ReadDir("builtin")
	if err != nil || len(entries) == 0 || len(entries) > 128 {
		return result, errBuiltinUnverified
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	whole := sha256.New()
	for _, entry := range entries {
		if entry.IsDir() {
			return result, errBuiltinUnverified
		}
		data, err := builtinFiles.ReadFile("builtin/" + entry.Name())
		if err != nil || len(data) > 1<<20 {
			return result, errBuiltinUnverified
		}
		sum := sha256.Sum256(data)
		result.Files = append(result.Files, builtinManifestFile{entry.Name(), int64(len(data)), hex.EncodeToString(sum[:])})
		whole.Write([]byte(entry.Name()))
		whole.Write([]byte{0})
		whole.Write(data)
		whole.Write([]byte{0})
	}
	result.SHA256 = hex.EncodeToString(whole.Sum(nil))
	return result, nil
}
func knownBuiltinManifest(prefix string) (builtinManifest, bool, error) {
	current, err := currentBuiltinManifest()
	if err != nil {
		return builtinManifest{}, false, err
	}
	if prefix == current.SHA256[:24] {
		return current, true, nil
	}
	entries, err := builtinManifests.ReadDir("builtin-manifests")
	if err != nil {
		return builtinManifest{}, false, errBuiltinUnverified
	}
	for _, entry := range entries {
		data, err := builtinManifests.ReadFile("builtin-manifests/" + entry.Name())
		if err != nil || len(data) > 32<<10 {
			return builtinManifest{}, false, errBuiltinUnverified
		}
		var saved builtinManifest
		if json.Unmarshal(data, &saved) != nil || saved.Version != 1 || len(saved.SHA256) != 64 ||
			len(saved.Files) == 0 || len(saved.Files) > 128 {
			return builtinManifest{}, false, errBuiltinUnverified
		}
		if prefix == saved.SHA256[:24] {
			return saved, false, nil
		}
	}
	return builtinManifest{}, false, errBuiltinUnverified
}
func builtinFileSum(manifest builtinManifest, name string) string {
	for _, file := range manifest.Files {
		if file.Name == name {
			return file.SHA256
		}
	}
	return ""
}

func readBuiltinFile(path string, file builtinManifestFile) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() != file.Bytes || file.Bytes < 0 ||
		file.Bytes > 1<<20 || !privatefile.Check(path) {
		return nil, errBuiltinUnverified
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errBuiltinUnverified
	}
	defer f.Close()
	held, err := f.Stat()
	if err != nil || !os.SameFile(before, held) {
		return nil, errBuiltinUnverified
	}
	data, err := io.ReadAll(io.LimitReader(f, file.Bytes+1))
	after, afterErr := os.Lstat(path)
	if err != nil || afterErr != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) ||
		before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) ||
		!privatefile.Check(path) || int64(len(data)) != file.Bytes {
		return nil, errBuiltinUnverified
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != file.SHA256 {
		return nil, errBuiltinUnverified
	}
	return data, nil
}
func verifyBuiltinDirectory(root, dir string, manifest builtinManifest) error {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || !filepath.IsAbs(root) || filepath.Clean(root) != root || canonical != root ||
		len(manifest.SHA256) != 64 || dir != filepath.Join(root, "runtime", "builtin", manifest.SHA256[:24]) {
		return errBuiltinUnverified
	}
	for _, path := range []string{root, filepath.Join(root, "runtime"), filepath.Join(root, "runtime", "builtin"), dir} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || !privatefile.CheckDirectory(path) {
			return errBuiltinUnverified
		}
	}
	before, err := os.Lstat(dir)
	if err != nil {
		return errBuiltinUnverified
	}
	handle, err := os.Open(dir)
	if err != nil {
		return errBuiltinUnverified
	}
	entries, err := handle.ReadDir(len(manifest.Files) + 1)
	handle.Close()
	if err != nil && !errors.Is(err, io.EOF) || len(entries) != len(manifest.Files) {
		return errBuiltinUnverified
	}
	names := map[string]bool{}
	for _, file := range manifest.Files {
		if file.Name == "" || filepath.Base(file.Name) != file.Name || strings.ContainsAny(file.Name, "/\\") ||
			names[file.Name] || (!strings.HasSuffix(file.Name, ".mjs") && file.Name != "catalog.json") {
			return errBuiltinUnverified
		}
		names[file.Name] = true
	}
	for _, entry := range entries {
		if !names[entry.Name()] || entry.IsDir() {
			return errBuiltinUnverified
		}
	}
	whole := sha256.New()
	total := int64(0)
	last := ""
	for _, file := range manifest.Files {
		if file.Name <= last {
			return errBuiltinUnverified
		}
		last = file.Name
		total += file.Bytes
		if total > 16<<20 {
			return errBuiltinUnverified
		}
		data, err := readBuiltinFile(filepath.Join(dir, file.Name), file)
		if err != nil {
			return err
		}
		whole.Write([]byte(file.Name))
		whole.Write([]byte{0})
		whole.Write(data)
		whole.Write([]byte{0})
	}
	after, err := os.Lstat(dir)
	if err != nil || !after.IsDir() || !os.SameFile(before, after) ||
		hex.EncodeToString(whole.Sum(nil)) != manifest.SHA256 {
		return errBuiltinUnverified
	}
	return nil
}

func verifyBuiltinReference(root, path, sum string, currentOnly bool) error {
	relative, err := filepath.Rel(filepath.Join(root, "runtime", "builtin"), path)
	if err != nil || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errBuiltinUnverified
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	if len(parts) != 2 || !builtinBundleName.MatchString(parts[0]) {
		return errBuiltinUnverified
	}
	manifest, current, err := knownBuiltinManifest(parts[0])
	if err != nil {
		return err
	}
	if expected := builtinFileSum(manifest, parts[1]); expected == "" || sum != expected {
		return errBuiltinUnverified
	}
	if err := verifyBuiltinDirectory(root, filepath.Dir(path), manifest); err != nil {
		return err
	}
	if currentOnly && !current {
		return errBuiltinOutdated
	}
	return nil
}

// Validate an existing destination before materialization. An unregistered
// edited/current-looking directory must not be silently repaired or adopted.
func safeBuiltinDestination(root string) error {
	manifest, err := currentBuiltinManifest()
	if err != nil {
		return err
	}
	for _, path := range []string{root, filepath.Join(root, "runtime"), filepath.Join(root, "runtime", "builtin")} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil || !info.IsDir() || !privatefile.CheckDirectory(path) {
			return errBuiltinUnverified
		}
	}
	dir := filepath.Join(root, "runtime", "builtin", manifest.SHA256[:24])
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return errBuiltinUnverified
	}
	return verifyBuiltinDirectory(root, dir, manifest)
}
