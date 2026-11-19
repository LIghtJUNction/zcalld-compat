package community

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
)

//go:embed builtin/*.mjs builtin/catalog.json
var builtinFiles embed.FS

// BuiltinScripts installs the helper's own optional provider adapters. The
// content-addressed directory keeps an old running host's relative imports
// together when the package is upgraded. It never depends on the checkout path.
func BuiltinScripts(root string) (map[string]string, error) {
	entries, err := builtinFiles.ReadDir("builtin")
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	h := sha256.New()
	files := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			return nil, errors.New("unexpected builtin resource directory")
		}
		name := entry.Name()
		data, err := builtinFiles.ReadFile("builtin/" + name)
		if err != nil {
			return nil, err
		}
		files[name] = data
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	dir := filepath.Join(root, "runtime", "builtin", hex.EncodeToString(h.Sum(nil))[:24])
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	paths := make(map[string]string, len(files))
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := Atomic(path, data, 0600); err != nil {
			return nil, err
		}
		paths[name] = path
	}
	return paths, nil
}
