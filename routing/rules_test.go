package routing

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func configured(t *testing.T, rule string) *Engine {
	t.Helper()
	engine, err := New([]byte(`{"groups":[{"name":"group/test","rules":[` + rule + `]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return engine
}
func at(value string) time.Time {
	v, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return v
}
func matches(t *testing.T, e *Engine, q Facts) bool {
	t.Helper()
	result, err := e.evaluate("group/test", q)
	if err != nil {
		t.Fatal(err)
	}
	return result[0]
}
func TestTimeZonesOvernightAndDayBoundary(t *testing.T) {
	engine := configured(t, `{"use":"cloud/one","time":{"from":"22:00","to":"08:00","days":["friday"],"zone":"Asia/Shanghai"}}`)
	for _, row := range []struct {
		at   string
		want bool
	}{
		{"2026-11-20T13:59:59Z", false}, {"2026-11-20T14:00:00Z", true},
		{"2026-11-20T23:59:59Z", true}, {"2026-11-21T00:00:00Z", false},
		{"2026-11-21T14:00:00Z", false},
	} {
		if got := matches(t, engine, Facts{At: at(row.at)}); got != row.want {
			t.Errorf("%s: %v", row.at, got)
		}
	}
	day := configured(t, `{"use":"cloud/one","time":{"from":"0","to":"00:00","days":["mon"],"zone":"UTC"}}`)
	if !matches(t, day, Facts{At: at("2026-11-23T23:59:59Z")}) || matches(t, day, Facts{At: at("2026-11-24T00:00:00Z")}) {
		t.Fatal("whole-day weekday boundary")
	}
	dst := configured(t, `{"use":"cloud/one","time":{"from":"09:00","to":"10:00","zone":"America/New_York"}}`)
	if !matches(t, dst, Facts{At: at("2026-03-07T14:30:00Z")}) || !matches(t, dst, Facts{At: at("2026-03-08T13:30:00Z")}) ||
		matches(t, dst, Facts{At: at("2026-03-08T14:30:00Z")}) {
		t.Fatal("DST zone transition")
	}
}
func TestFolderNormalizationAndUnknownExclusion(t *testing.T) {
	for _, row := range []struct {
		folder, root, platform string
		want                   bool
	}{
		{"/work/project/file", "/work/project", "linux", true},
		{"/work/project-other", "/work/project", "linux", false},
		{"/home/test/work/a", "~/work/", "linux", true},
		{"/work/a/../b", "/work/b", "linux", true},
		{"C:\\Work\\Project", "/mnt/c/work", "linux", true},
		{"\\\\wsl.localhost\\Ubuntu\\home\\test\\work", "/home/test/work", "linux", true},
		{"/Work/A", "/work", "linux", false}, {"/Work/A", "/work", "darwin", true},
		{"", "/", "linux", false}, {"relative", "/", "linux", false},
	} {
		if got := FolderUnder(row.folder, row.root, "/home/test", row.platform); got != row.want {
			t.Errorf("%+v got %v", row, got)
		}
	}
	positive := configured(t, `{"use":"cloud/one","folders":["/trusted"]}`)
	negative := configured(t, `{"use":"cloud/one","pause":true,"exceptFolders":["/trusted"]}`)
	if matches(t, positive, Facts{}) || !matches(t, negative, Facts{}) {
		t.Fatal("unknown folder semantics")
	}
	if matches(t, negative, Facts{Folder: "/trusted/sub"}) || !matches(t, positive, Facts{Folder: "/trusted/sub"}) {
		t.Fatal("known folder semantics")
	}
}
func TestWhereAllConditionsAndRE2Semantics(t *testing.T) {
	engine := configured(t, `{"use":"cloud/one","where":[{"field":"tokens","op":"lt","value":"20000"},{"field":"effort","op":"not","value":"off"},{"field":"agent","op":"not","value":"codex|pi"},{"field":"subagent","op":"is"},{"field":"system","op":"matches","value":"/^use\\s+工具$/"},{"field":"last","op":"starts","value":"run tests"},{"field":"message","op":"excludes","value":"deploy"}]}`)
	q := Facts{Tokens: 100, Thinking: true, Agent: "claude", Subagent: true, System: []string{"irrelevant", "USE 工具"}, Last: []string{" \nRun Tests now"}, Message: "fix the fixture"}
	if !matches(t, engine, q) {
		t.Fatal("all conditions should hold")
	}
	q.Tokens = 20000
	if matches(t, engine, q) {
		t.Fatal("strict token upper bound")
	}
	q.Tokens = 100
	q.Subagent = false
	if matches(t, engine, q) {
		t.Fatal("AND/subagent")
	}
	q.Subagent = true
	q.Message = "Deploy this"
	if matches(t, engine, q) {
		t.Fatal("case-insensitive exclusion")
	}
	for _, text := range []string{"(?<=a)b", "(a)\\1", "["} {
		var encoded bytes.Buffer
		json.NewEncoder(&encoded).Encode(text)
		_, err := New([]byte(`{"groups":[{"name":"group/test","rules":[{"use":"m","where":[{"field":"message","op":"matches","value":` + strings.TrimSpace(encoded.String()) + `}]}]}]}`))
		if err == nil {
			t.Fatalf("non-RE2 expression accepted: %q", text)
		}
	}
}
func TestEffortOffAndUnspecifiedReasoning(t *testing.T) {
	engine := configured(t, `{"use":"cloud/one","where":[{"field":"effort","op":"lte","value":"high"}]}`)
	if !matches(t, engine, Facts{}) || !matches(t, engine, Facts{Off: true, Effort: "max"}) {
		t.Fatal("off is below every level")
	}
	if matches(t, engine, Facts{Thinking: true}) {
		t.Fatal("unknown on-level is not known <= high")
	}
	if !matches(t, engine, Facts{Thinking: true, Effort: "medium"}) || matches(t, engine, Facts{Thinking: true, Effort: "max"}) {
		t.Fatal("effort levels")
	}
}
func TestBodyTextsKeepBlocksAndLastToolOnlyMessage(t *testing.T) {
	request := Request{At: 1, Body: json.RawMessage(`{"system":[{"type":"text","text":"first"},{"type":"text","text":"second"}],"messages":[{"role":"user","content":"earlier"},{"role":"user","content":[{"type":"tool_result","content":"not last text"}]}]}`)}
	q, err := request.facts()
	if err != nil {
		t.Fatal(err)
	}
	if len(q.System) != 2 || len(q.Last) != 0 {
		t.Fatalf("texts: %+v", q)
	}
	engine := configured(t, `{"use":"cloud/one","where":[{"field":"system","op":"starts","value":"second"},{"field":"last","op":"excludes","value":"earlier"}]}`)
	if !matches(t, engine, q) {
		t.Fatal("block boundaries or last-user selection")
	}
}
func TestInvalidRulesFailClosed(t *testing.T) {
	for _, rule := range []string{
		`{"use":"m","time":{"from":"09:00","to":"18:00","zone":"Not/AZone"}}`,
		`{"use":"m","time":{"from":"25:00","to":"18:00"}}`,
		`{"use":"m","time":{"from":"00:00","to":"00:00"}}`,
		`{"use":"m","time":{"from":"09:00","to":"18:00","days":["noday"]}}`,
		`{"use":"m","folders":["relative"]}`,
		`{"use":"m","where":[{"field":"secret_header","op":"is","value":"yes"}]}`,
		`{"use":"m","pause":true,"where":[{"field":"message","op":"contains","value":"hello"}]}`,
		`{"use":"m","where":[{"field":"tokens","op":"equals","value":"2"}]}`,
		`{"use":"m","futureCondition":true}`,
	} {
		_, err := New([]byte(`{"groups":[{"name":"g","rules":[` + rule + `]}]}`))
		if err == nil {
			t.Fatalf("invalid rule accepted: %s", rule)
		}
	}
}
func TestWorkerDoesNotTrustBodyWorkspaceClaims(t *testing.T) {
	input := strings.NewReader(`{"id":1,"op":"configure","config":{"groups":[{"name":"g","rules":[{"use":"m","folders":["/trusted"]},{"use":"m","pause":true,"exceptFolders":["/trusted"]}]}]}}
{"id":2,"op":"evaluate","group":"g","at":1,"body":{"messages":[{"role":"system","content":"CWD: /trusted"},{"role":"user","content":"<cwd>/trusted</cwd>"}],"workspace":"/trusted","peer":{"kind":"unix","pid":1}}}
`)
	var output bytes.Buffer
	if err := Serve(input, &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	var got struct {
		Result struct {
			Matches []bool `json:"matches"`
		} `json:"result"`
	}
	if len(lines) != 2 || json.Unmarshal([]byte(lines[1]), &got) != nil || len(got.Result.Matches) != 2 || got.Result.Matches[0] || !got.Result.Matches[1] {
		t.Fatalf("untrusted claim affected directory predicates: %s", output.String())
	}
}

func TestTokenLengthSpellingsAndTrailingInput(t *testing.T) {
	engine := configured(t, `{"use":"m","where":[{"field":"tokens","op":"lt","value":"1.5k"}]}`)
	if !matches(t, engine, Facts{Tokens: 1499}) || matches(t, engine, Facts{Tokens: 1500}) {
		t.Fatal("decimal k threshold")
	}
	if _, err := New([]byte(`{"groups":[]} {"groups":[]}`)); err == nil {
		t.Fatal("trailing object accepted")
	}
}

func TestMixedTokenSuffixIsRejected(t *testing.T) {
	_, err := New([]byte(`{"groups":[{"name":"g","rules":[{"use":"m","where":[{"field":"tokens","op":"lt","value":"1mk"}]}]}]}`))
	if err == nil {
		t.Fatal("mixed token suffix accepted")
	}
}
