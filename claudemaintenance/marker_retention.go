package claudemaintenance

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// A failed finalizer must retain the real marker handle, not only a flag. It
// also prevents this process from selecting that native directory again. This
// is process-lifetime ownership: an exiting one-shot cannot hold an OS lock
// forever. The maintenance engine separately keeps unknown generation intents
// on disk and never automatically replays them.
var retainedRunMarkers = struct {
	sync.Mutex
	owners map[string][]func()
}{owners: make(map[string][]func())}

func runMarkerKey(a account) string {
	key := filepath.Clean(a.ConfigDir)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	return key
}

func runMarkerRetained(a account) bool {
	retainedRunMarkers.Lock()
	defer retainedRunMarkers.Unlock()
	return len(retainedRunMarkers.owners[runMarkerKey(a)]) != 0
}

func finishRunMarker(a account, release func(), err error) {
	if !errors.Is(err, errProcessDrain) {
		release()
		return
	}
	retainedRunMarkers.Lock()
	defer retainedRunMarkers.Unlock()
	key := runMarkerKey(a)
	retainedRunMarkers.owners[key] = append(retainedRunMarkers.owners[key], release)
}
