package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
	"github.com/LIghtJUNction/zcalld-compat/privacy"
)

func TestStdioAuditFlushAndEOFAcknowledgement(t *testing.T) {
	dir := t.TempDir()
	key, settings := filepath.Join(dir, "redact.key"), filepath.Join(dir, "settings.json")
	for p, b := range map[string][]byte{
		key:      bytes.Repeat([]byte{7}, 32),
		settings: []byte(`{"redactPersonal":true,"redactModelOff":true}`),
	} {
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
		if err := privatefile.Protect(p, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var input bytes.Buffer
	encoder := json.NewEncoder(&input)
	context := privacy.Context{Agent: "stdio-agent", Session: "stdio-session", What: "/v1/responses"}
	lines := []request{
		{ID: 1, Op: "configure", Config: privacy.Config{Settings: settings, Key: key}},
		{ID: 2, Op: "mask", Body: []byte(`{"messages":[]}`)},
		{ID: 3, Op: "mask", Body: []byte(`{"input":"stdio.person@acme.io"}`), Context: context},
		{ID: 4, Op: "flush"},
		{ID: 5, Op: "mask", Body: []byte(`{"input":"ordinary harmless text"}`), Context: context},
		{ID: 6, Op: "mask", Body: []byte(`{"input":"stdio.person@acme.io"}`), Context: context},
	}
	for _, line := range lines {
		if err := encoder.Encode(line); err != nil {
			t.Fatal(err)
		}
	}
	// Old clients can omit context or send null. Invalid types are rejected.
	input.WriteString("{\"id\":7,\"op\":\"mask\",\"context\":{\"agent\":123}}\n")
	var output, diagnostic bytes.Buffer
	if code := run(&input, &output, &diagnostic); code != 0 || diagnostic.Len() != 0 {
		t.Fatalf("worker exit %d: %s", code, diagnostic.String())
	}
	scan := bufio.NewScanner(&output)
	replies := map[uint64]answer{}
	for scan.Scan() {
		var a answer
		if err := json.Unmarshal(scan.Bytes(), &a); err != nil {
			t.Fatal(err)
		}
		replies[a.ID] = a
	}
	if replies[4].Result != true || replies[0].Error != "invalid worker request" {
		t.Fatal("flush/context protocol did not acknowledge correctly")
	}
	if len(replies) != 7 {
		t.Fatal("worker lost replies")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "history", "*.jsonl"))
	if len(files) != 1 {
		t.Fatal("wrong daily record count")
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(data, []byte{'\n'}) != 2 || bytes.Contains(data, []byte("stdio.person@acme.io")) {
		t.Fatal("audit omitted or leaked request values")
	}
	countsPath := filepath.Join(dir, "history", "checked-"+time.Now().Local().Format("2006-01-02")+".json")
	countsData, err := os.ReadFile(countsPath)
	if err != nil {
		t.Fatal(err)
	}
	var counts map[string]map[string]int
	if json.Unmarshal(countsData, &counts) != nil || counts["stdio-agent"]["looked"] != 3 || counts["stdio-agent"]["masked"] != 2 {
		t.Fatal("EOF did not flush counts after explicit flush")
	}
	if !privatefile.Check(files[0]) || !privatefile.Check(countsPath) {
		t.Fatal("worker audit data was not private")
	}
}
