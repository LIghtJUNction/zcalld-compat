package claudemaintenance

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCapabilityFailurePreservesDrainWithoutLeakingUnderlyingError(t *testing.T) {
	const canary = "https://synthetic.invalid/?credential=DO-NOT-OUTPUT"
	for _, input := range []error{errors.New(canary), errors.Join(errors.New(canary), errProcessDrain)} {
		got := capabilityFailure(input)
		if strings.Contains(got.Error(), canary) {
			t.Fatal("capability failure leaked the underlying error")
		}
		if errors.Is(got, errProcessDrain) != errors.Is(input, errProcessDrain) {
			t.Fatal("capability failure lost process ownership classification")
		}
	}
}

func TestProviderRequiresToolBridgeAndOwnedDrainWithoutChangingMaintenance(t *testing.T) {
	for _, tc := range []struct {
		name          string
		bridge, drain bool
	}{
		{"legacy", false, false}, {"bridge-only", true, false},
		{"drain-only", false, true}, {"complete", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(map[string]any{
				"schema_version": 1, "source_revision": referenceRevision,
				"interactive": true, "claude_binary_absolute": true,
				"config_dir_explicit": true, "profile_path_explicit": true,
				"stream_json": true, "print_fallback": false, "wsl": false,
				"mcp_tool_bridge": tc.bridge, "owned_drain": tc.drain,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err = validateCapabilities(b, false); err != nil {
				t.Fatal("changed ordinary maintenance capability contract", err)
			}
			if got := validateCapabilities(b, true); (got == nil) != (tc.bridge && tc.drain) {
				t.Fatal("provider accepted an incomplete helper", got)
			}
		})
	}
}

func TestUnconfirmedMaintenanceDrainRetainsTheRealMarkerAcrossGC(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "unconfirmed"}[unknown], func(t *testing.T) {
			f := nativeFixture(t)
			key := bindNative(t, f)
			_, a, _, err := (Host{Root: f.root}).selected(key)
			if err != nil {
				t.Fatal(err)
			}
			release, err := markRun(a)
			if err != nil {
				t.Fatal(err)
			}
			markerKey := runMarkerKey(a)
			t.Cleanup(func() {
				retainedRunMarkers.Lock()
				owners := retainedRunMarkers.owners[markerKey]
				delete(retainedRunMarkers.owners, markerKey)
				retainedRunMarkers.Unlock()
				for _, close := range owners {
					close()
				}
			})
			paths, err := filepath.Glob(a.ConfigDir + ".run-*.lock")
			if err != nil || len(paths) != 1 {
				t.Fatal(paths, err)
			}
			var result error
			if unknown {
				result = errors.Join(errors.New("synthetic finalizer error"), errProcessDrain)
			}
			finishRunMarker(a, release, result)
			release = nil
			for range 3 {
				runtime.GC()
			}
			if !unknown {
				if _, err = os.Lstat(paths[0]); !os.IsNotExist(err) {
					t.Fatal("confirmed drain kept its marker", err)
				}
				if runMarkerRetained(a) {
					t.Fatal("confirmed drain was quarantined")
				}
				return
			}
			if _, _, _, err = (Host{Root: f.root}).selected(key); !errors.Is(err, errProcessDrain) {
				t.Fatal("unconfirmed account became selectable", err)
			}
			resultAction, actionErr := (Host{Root: f.root}).Warmup(t.Context(), key, []string{"7 days"})
			if resultAction.Status != "unknown" || !errors.Is(actionErr, errProcessDrain) {
				t.Fatal("unconfirmed owner was misclassified as safe to replay", resultAction, actionErr)
			}
			if len(generations(t, f)) != 0 {
				t.Fatal("quarantined owner launched the fake generation")
			}
			second, err := os.OpenFile(paths[0], os.O_RDWR, 0)
			if err != nil {
				t.Fatal("unconfirmed drain lost marker", err)
			}
			defer second.Close()
			unlock, err := lockMarker(second)
			if err == nil {
				unlock()
				t.Fatal("marker handle was released while the owner was alive")
			}
		})
	}
}
