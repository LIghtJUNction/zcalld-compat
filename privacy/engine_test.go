package privacy

import (
	"bytes"
	"encoding/json"
	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testEngine(t *testing.T, s map[string]any) *Engine {
	t.Helper()
	d := t.TempDir()
	p := filepath.Join(d, "settings.json")
	k := filepath.Join(d, "redact.key")
	s["redactModelOff"] = true
	b, _ := json.Marshal(s)
	os.WriteFile(p, b, 0600)
	os.WriteFile(k, bytes.Repeat([]byte{1}, 32), 0600)
	if err := privatefile.Protect(k, 0600); err != nil {
		t.Fatal(err)
	}
	keyLock.Lock()
	keyPath = ""
	keyLock.Unlock()
	e, err := New(Config{Settings: p, Key: k})
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func TestMaskAndRestoreSplitToolArguments(t *testing.T) {
	e := testEngine(t, map[string]any{"redact": true, "redactPersonal": true})
	input := []byte("{\"model\":\"x\",\"messages\":[{\"role\":\"user\",\"content\":\"API_KEY=sk-test-abcdefgh1234567890 and jane.doe@acme.io\"}]}")
	r, err := e.Mask(input, "test")
	if err != nil || r.Count == 0 {
		t.Fatalf("mask failed: %v", err)
	}
	if bytes.Contains(r.Body, []byte("jane.doe@acme.io")) {
		t.Fatal("email leaked")
	}
	var b map[string]any
	json.Unmarshal(r.Body, &b)
	text := b["messages"].([]any)[0].(map[string]any)["content"].(string)
	restored := NewStream("application/json")
	response, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": []any{map[string]any{"function": map[string]any{"arguments": text}}}}}}})
	out, err := restored.Write(response, true)
	if err != nil || !bytes.Contains(out, []byte("jane.doe@acme.io")) {
		t.Fatalf("restoration failed: %v", err)
	}
	start := strings.Index(text, "{{")
	end := strings.Index(text[start:], "}}") + start + 2
	tag := text[start:end]
	stream := NewStream("text/event-stream")
	var all []byte
	for _, piece := range []string{tag[:len(tag)/2], tag[len(tag)/2:]} {
		event, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": piece}}}})
		chunk := append(append([]byte("data: "), event...), []byte("\n\n")...)
		out, err = stream.Write(chunk, false)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, out...)
	}
	out, err = stream.Write([]byte("data: [DONE]\n\n"), true)
	all = append(all, out...)
	if err != nil || bytes.Contains(all, []byte("{{")) {
		t.Fatalf("split placeholder left: %v", err)
	}
}
func TestBlockFailClosedAndSettingsReload(t *testing.T) {
	e := testEngine(t, map[string]any{"redactPersonal": true, "redactActions": map[string]string{"EMAIL": "block"}})
	r, err := e.Mask([]byte("{\"messages\":[{\"content\":\"jane.doe@acme.io\"}]}"), "test")
	if err != nil || !r.Blocked || len(r.Body) > 0 {
		t.Fatal("blocked value could escape")
	}
	os.WriteFile(e.config.Settings, []byte("{"), 0600)
	if _, err = e.Mask([]byte("{}"), "test"); err == nil {
		t.Fatal("invalid settings did not fail closed")
	}
}

func TestDisabledOverridePreservesSettings(t *testing.T) {
	e := testEngine(t, map[string]any{"redact": true, "redactPersonal": true, "zcalldEnabled": false, "redactWords": []string{"synthetic-secret"}})
	before, _ := os.ReadFile(e.config.Settings)
	input := []byte("{\"messages\":[{\"content\":\"synthetic-secret jane.doe@acme.io\"}]}")
	result, err := e.Mask(input, "test")
	if err != nil || !bytes.Equal(result.Body, input) || result.Enabled {
		t.Fatal("disabled privacy changed a request")
	}
	after, _ := os.ReadFile(e.config.Settings)
	if !bytes.Equal(before, after) {
		t.Fatal("disabling erased user rules")
	}
}
func TestExplicitModelFailsClosed(t *testing.T) {
	e := testEngine(t, map[string]any{"redactPersonal": true, "redactModel": true, "redactModelName": "astrlink-guard"})
	e.config.ModelDir = t.TempDir()
	if _, err := e.Mask([]byte("{\"messages\":[{\"content\":\"please keep this local\"}]}"), "test"); err == nil {
		t.Fatal("missing explicit model did not hold request back")
	}
}
func TestInstalledModelActuallyClassifies(t *testing.T) {
	dir := os.Getenv("ZCALLD_TEST_MODEL_DIR")
	if dir == "" {
		t.Skip("explicit local model fixture is not configured")
	}
	e := testEngine(t, map[string]any{"redactPersonal": true, "redactModel": true, "redactModelName": "astrlink-guard"})
	e.config.ModelDir = dir
	result, err := e.Mask([]byte("{\"messages\":[{\"content\":\"联系人：张三，电话 13800138000\"}]}"), "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.Model != "astrlink-guard" || bytes.Contains(result.Body, []byte("张三")) {
		t.Fatal("local model was not applied")
	}
}
