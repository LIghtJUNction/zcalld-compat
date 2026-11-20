package community

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Exercise the actual embedded host with a synthetic local initializer.
// No credentials or network calls are permitted in this fixture.
func TestNativeReadySharesStrictInitAndCancelsOnlyOneWaiter(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("Bun is optional; native readiness fixture requires Bun")
	}
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "strict_failure"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "initializer.mjs")
			preload := filepath.Join(root, "network-denied.mjs")
			source := "import {existsSync,writeFileSync} from 'node:fs';\n" +
				"export default async()=>{writeFileSync(process.env.ZCALLD_READY_STARTED,'1');" +
				"while(!existsSync(process.env.ZCALLD_READY_RELEASE))await new Promise(r=>setTimeout(r,5));" +
				"if(process.env.ZCALLD_READY_FAIL==='1')throw new Error('synthetic init failure');return {}};\n"
			if err := os.WriteFile(target, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(preload, []byte("globalThis.fetch=async()=>{throw new Error('network forbidden in readiness fixture')};\n"), 0600); err != nil {
				t.Fatal(err)
			}
			host, err := filepath.Abs(filepath.Join("vendor", "host.js"))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bun, "--preload", preload, host)
			cmd.Env = append(os.Environ(), "ZCALLD_READY_STARTED="+filepath.Join(root, "started"),
				"ZCALLD_READY_RELEASE="+filepath.Join(root, "release"))
			if fail {
				cmd.Env = append(cmd.Env, "ZCALLD_READY_FAIL=1")
			}
			in, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			wait := make(chan error, 1)
			go func() { wait <- cmd.Wait() }()
			defer func() {
				_ = in.Close()
				cancel()
				<-wait
			}()
			frames := make(chan map[string]any, 16)
			go func() {
				defer close(frames)
				scan := bufio.NewScanner(out)
				for scan.Scan() {
					var frame map[string]any
					if json.Unmarshal(scan.Bytes(), &frame) == nil {
						frames <- frame
					}
				}
			}()
			send := func(v any) {
				t.Helper()
				if err := json.NewEncoder(in).Encode(v); err != nil {
					t.Fatal(err)
				}
			}
			send(map[string]any{"id": 1, "method": "init", "params": map[string]any{
				"authPath": filepath.Join(root, "absent-auth.json"), "directory": root,
				"nativeStrict": true, "plugins": []any{map[string]any{"spec": "fixture", "target": target}},
			}})
			until := time.Now().Add(2 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(root, "started")); err == nil {
					break
				}
				if time.Now().After(until) {
					t.Fatal("synthetic initializer did not start")
				}
				time.Sleep(time.Millisecond)
			}
			send(map[string]any{"id": 2, "method": "nativeReady", "params": map[string]any{}})
			send(map[string]any{"id": 3, "method": "nativeReady", "params": map[string]any{}})
			send(map[string]any{"method": "abort", "params": map[string]any{"id": 2}})
			select {
			case frame := <-frames:
				t.Fatalf("readiness replied before initializer settled: %v", frame)
			case <-time.After(20 * time.Millisecond):
			}
			if err := os.WriteFile(filepath.Join(root, "release"), []byte("go"), 0600); err != nil {
				t.Fatal(err)
			}
			seen := map[int]bool{}
			for len(seen) < 2 {
				select {
				case frame, ok := <-frames:
					if !ok {
						t.Fatal("host exited before readiness result")
					}
					id, _ := frame["id"].(float64)
					if id != 1 && id != 3 {
						t.Fatalf("cancelled waiter or unexpected frame: %v", frame)
					}
					_, hasError := frame["error"]
					if hasError != fail {
						t.Fatalf("strict initialization result was not shared: %v", frame)
					}
					seen[int(id)] = true
				case <-ctx.Done():
					t.Fatal("host readiness deadline")
				}
			}
			send(map[string]any{"id": 4, "method": "nativeReady", "params": map[string]any{}})
			select {
			case frame := <-frames:
				id, _ := frame["id"].(float64)
				_, hasError := frame["error"]
				if id != 4 || hasError != fail {
					t.Fatalf("late waiter did not reuse settled readiness: %v", frame)
				}
			case <-ctx.Done():
				t.Fatal("settled readiness deadline")
			}
		})
	}
}
