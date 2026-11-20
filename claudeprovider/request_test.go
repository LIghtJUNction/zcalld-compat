package claudeprovider

import (
	"encoding/json"
	"strings"
	"testing"
)

func fixtureRequest(t *testing.T) *Request {
	t.Helper()
	r, err := parseRequest([]byte(`{"model":"haiku","max_tokens":256,"messages":[{"role":"user","content":"hello"}]}`), "haiku")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRequestDoesNotSilentlyDropUnsupportedControls(t *testing.T) {
	for _, field := range []string{`"temperature":0`, `"thinking":{"type":"enabled","budget_tokens":1000}`, `"stop_sequences":["x"]`, `"metadata":{"user_id":"x"}`, `"output_config":{"format":{"type":"json_schema","schema":{}}}`} {
		raw := `{"model":"haiku","max_tokens":256,"messages":[{"role":"user","content":"x"}],` + field + `}`
		if _, err := parseRequest([]byte(raw), "haiku"); err == nil {
			t.Fatalf("accepted unsupported control %s", field)
		}
	}
	for _, raw := range []string{
		`{"model":"haiku","model":"opus","max_tokens":256,"messages":[{"role":"user","content":"x"}]}`,
		`{"model":"haiku","max_tokens":256,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://invalid.example/image"}}]}]}`,
		`{"model":"haiku","max_tokens":256,"messages":[{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]}]}`,
	} {
		if _, err := parseRequest([]byte(raw), "haiku"); err == nil {
			t.Fatal("accepted ambiguous or unsupported content")
		}
	}
	if _, err := parseRequest(encode(fixtureRequest(t)), "opus"); err == nil {
		t.Fatal("ignored configured model")
	}
}

func TestInlineImagesAndHistoryRetainTypedValues(t *testing.T) {
	r := fixtureRequest(t)
	r.Messages = []Message{
		{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"old","name":"search","input":{"n":9007199254740993}}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"old","is_error":true,"content":[{"type":"text","text":"not found"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]`)},
	}
	checked, err := parseRequest(encode(r), "haiku")
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := checked.prompt()
	if err != nil {
		t.Fatal(err)
	}
	b := string(encode(blockValues(prompt)))
	for _, value := range []string{"9007199254740993", "is_error=true", "not found", "image/png", "aGk="} {
		if !strings.Contains(b, value) {
			t.Fatalf("lost value %s", value)
		}
	}
	opaque := *r
	opaque.Messages = append([]Message{{Role: "assistant", Content: json.RawMessage(`[{"type":"thinking","thinking":"opaque","signature":"signature"}]`)}}, r.Messages...)
	if _, err = opaque.prompt(); err == nil {
		t.Fatal("replayed opaque reasoning into a new native session")
	}
}

func TestToolAliasesAndCanonicalNumbersDoNotCollide(t *testing.T) {
	names := []string{"search", "search/path", "search.path", "mcp__magpie__already", "x_0123456789ab"}
	seen := map[string]bool{}
	for _, name := range names {
		alias := bridgeName(name)
		if seen[alias] || !plainTool.MatchString(alias) {
			t.Fatalf("unsafe alias %q", alias)
		}
		seen[alias] = true
	}
	if got := string(canonical([]byte(`{"n":9007199254740993,"a":1}`))); !strings.Contains(got, "9007199254740993") {
		t.Fatal("rounded tool argument")
	}
}
