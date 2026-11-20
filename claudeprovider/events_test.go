package claudeprovider

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

type memorySink struct {
	status int
	kind   string
	bytes.Buffer
}

func (s *memorySink) Head(status int, kind string) error {
	s.status = status
	s.kind = kind
	return nil
}
func (s *memorySink) Write(b []byte) error { _, err := s.Buffer.Write(b); return err }

func streamFixture(t *testing.T, r *Request, sink *memorySink, tool bool) *turn {
	t.Helper()
	turn, err := newTurn(r, sink)
	if err != nil {
		t.Fatal(err)
	}
	events := []string{
		`{"type":"message_start","message":{"id":"native-id","model":"haiku","usage":{"input_tokens":12,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		`{"type":"content_block_stop","index":0}`,
	}
	if tool {
		events = append(events,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"native-tool","name":"mcp__magpie__search","input":{}}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"test\"}"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`,
		)
	} else {
		events = append(events, `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`)
	}
	events = append(events, `{"type":"message_stop"}`)
	for _, event := range events {
		if err = turn.stream([]byte(event)); err != nil {
			t.Fatal(err)
		}
	}
	return turn
}

func TestNativeEventsProduceJSONAndSSEWithOneCompletion(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			r := fixtureRequest(t)
			r.Stream = stream
			sink := &memorySink{}
			turn := streamFixture(t, r, sink, false)
			if err := turn.assistant([]byte(`{"id":"native-id","model":"haiku","content":[{"type":"text","text":"hello"}]}`)); err != nil {
				t.Fatal(err)
			}
			if err := turn.finish(false); err != nil {
				t.Fatal(err)
			}
			if sink.status != 200 || !strings.Contains(sink.String(), "hello") {
				t.Fatal("missing response")
			}
			if stream {
				if strings.Count(sink.String(), "event: message_stop\n") != 1 {
					t.Fatal("duplicate completion")
				}
			} else {
				var value map[string]any
				if json.Unmarshal(sink.Bytes(), &value) != nil || value["stop_reason"] != "end_turn" {
					t.Fatal("wrong JSON result")
				}
			}
			if turn.finish(false) == nil {
				t.Fatal("allowed duplicate completion")
			}
		})
	}
}

func TestToolCapabilitiesReplaceNativeIDsAndValidateLaterTranscript(t *testing.T) {
	r := fixtureRequest(t)
	r.Tools = []Tool{{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	sink := &memorySink{}
	turn := streamFixture(t, r, sink, true)
	if len(turn.calls) != 1 || !capabilityID.MatchString(turn.calls[0].ID) || turn.calls[0].ID == turn.calls[0].NativeID {
		t.Fatal("tool capability missing")
	}
	if err := turn.assistant([]byte(`{"id":"native-id","model":"haiku","content":[{"type":"tool_use","id":"native-tool","name":"mcp__magpie__search","input":{"q":"test"}}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := turn.finish(true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sink.String(), "native-tool") {
		t.Fatal("native identity leaked instead of capability")
	}
	if err := turn.validateTranscript([]Block{{Type: "tool_use", ID: "native-tool", Name: "mcp__magpie__search", Input: json.RawMessage(`{"q":"changed"}`)}}); err == nil {
		t.Fatal("accepted changed transcript tool arguments")
	}
}

func TestIncompleteOrOversizedOutputDoesNotClaimCompletion(t *testing.T) {
	r := fixtureRequest(t)
	sink := &memorySink{}
	turn, _ := newTurn(r, sink)
	if turn.stream([]byte(`{"type":"message_stop"}`)) == nil {
		t.Fatal("accepted unconfirmed stop")
	}
	turn, _ = newTurn(r, sink)
	turn.bytes = maxOutput
	if turn.assistant([]byte(`{"id":"x","model":"haiku","content":[{"type":"text","text":"x"}]}`)) == nil {
		t.Fatal("nonstream output bypassed bound")
	}
}
