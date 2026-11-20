package claudeprovider

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

type responseSink interface {
	Head(int, string) error
	Write([]byte) error
}

type toolCall struct {
	NativeID, ID, Name, Alias string
	Arguments                 json.RawMessage
	response                  chan toolAnswer
	attached                  bool
}
type toolAnswer struct {
	Content []Block `json:"content"`
	IsError bool    `json:"isError,omitempty"`
}

type turn struct {
	request                                  *Request
	sink                                     responseSink
	id                                       string
	nativeID                                 string
	started, completed, finished, transcript bool
	streamed                                 bool
	blocks                                   []Block
	closed                                   []bool
	partials                                 map[int]string
	calls                                    []*toolCall
	usage                                    map[string]json.RawMessage
	stopReason                               string
	bytes                                    int
	transcriptBlocks                         [][]Block
}

func randomID(prefix string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}
func newTurn(r *Request, sink responseSink) (*turn, error) {
	id, err := randomID("msg_zc_")
	if err != nil {
		return nil, err
	}
	return &turn{request: r, sink: sink, id: id, partials: map[int]string{}, usage: map[string]json.RawMessage{"input_tokens": json.RawMessage("0"), "output_tokens": json.RawMessage("0")}}, nil
}

func (t *turn) event(kind string, v any) error {
	if !t.request.Stream {
		return nil
	}
	b := encode(v)
	t.bytes += len(b)
	if t.bytes > maxOutput {
		return errors.New("Claude response exceeded its limit")
	}
	return t.sink.Write(append(append([]byte("event: "+kind+"\ndata: "), b...), []byte("\n\n")...))
}

func (t *turn) start() error {
	if t.started {
		return nil
	}
	t.started = true
	if !t.request.Stream {
		return nil
	}
	if err := t.sink.Head(200, "text/event-stream"); err != nil {
		return err
	}
	return t.event("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": t.id, "type": "message", "role": "assistant", "model": t.request.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": t.usage}})
}

func (t *turn) usageFrom(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return invalid()
	}
	for _, name := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
		if v, ok := values[name]; ok {
			var n uint64
			if json.Unmarshal(v, &n) != nil {
				return errors.New("Claude usage metadata is invalid")
			}
			t.usage[name] = v
		}
	}
	return nil
}

func (t *turn) addBlock(block Block) (int, error) {
	if len(t.blocks) >= 4096 {
		return 0, errors.New("Claude response has too many blocks")
	}
	if block.Type == "tool_use" {
		if t.request.ToolChoice != nil && t.request.ToolChoice.Type == "none" {
			return 0, errors.New("Claude attempted a disabled tool")
		}
		alias := strings.TrimPrefix(block.Name, "mcp__magpie__")
		name := ""
		for _, tool := range t.request.Tools {
			if bridgeName(tool.Name) == alias {
				name = tool.Name
				break
			}
		}
		if name == "" || block.ID == "" || len(block.ID) > 256 {
			return 0, errors.New("Claude attempted an unregistered tool")
		}
		for _, c := range t.calls {
			if c.NativeID == block.ID {
				return 0, errors.New("Claude repeated a tool call identity")
			}
		}
		id, err := randomID("toolu_zc_")
		if err != nil {
			return 0, err
		}
		call := &toolCall{NativeID: block.ID, ID: id, Name: name, Alias: alias, Arguments: block.Input, response: make(chan toolAnswer, 1)}
		t.calls = append(t.calls, call)
		block.ID = id
		block.Name = name
	}
	index := len(t.blocks)
	t.blocks = append(t.blocks, block)
	t.closed = append(t.closed, false)
	if err := t.start(); err != nil {
		return 0, err
	}
	if err := t.event("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": blockValue(block)}); err != nil {
		return 0, err
	}
	return index, nil
}

func (t *turn) stopBlock(index int) error {
	if index < 0 || index >= len(t.blocks) || t.closed[index] {
		return errors.New("Claude content block order is invalid")
	}
	b := &t.blocks[index]
	if b.Type == "tool_use" {
		if part, ok := t.partials[index]; ok {
			b.Input = json.RawMessage(part)
		}
		var args map[string]json.RawMessage
		if unique(b.Input) != nil || json.Unmarshal(b.Input, &args) != nil || args == nil {
			return errors.New("Claude tool arguments are invalid")
		}
		for _, call := range t.calls {
			if call.ID == b.ID {
				call.Arguments = append(json.RawMessage(nil), b.Input...)
			}
		}
	}
	t.closed[index] = true
	return t.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}

func (t *turn) stream(raw json.RawMessage) error {
	t.bytes += len(raw)
	if t.bytes > maxOutput {
		return errors.New("Claude response exceeded its limit")
	}
	var e struct {
		Type    string          `json:"type"`
		Index   int             `json:"index"`
		Message json.RawMessage `json:"message"`
		Block   Block           `json:"content_block"`
		Delta   json.RawMessage `json:"delta"`
		Usage   json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return errors.New("Claude stream is invalid")
	}
	if t.completed {
		return errors.New("Claude emitted a second message before the tool boundary")
	}
	t.streamed = true
	switch e.Type {
	case "message_start":
		if t.started {
			return errors.New("Claude emitted duplicate message metadata")
		}
		var m struct {
			ID    string          `json:"id"`
			Model string          `json:"model"`
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(e.Message, &m) != nil || m.ID == "" || len(m.ID) > 256 || m.Model == "<synthetic>" {
			return invalid()
		}
		t.nativeID = m.ID
		if err := t.usageFrom(m.Usage); err != nil {
			return err
		}
		return t.start()
	case "content_block_start":
		if e.Index != len(t.blocks) {
			return errors.New("Claude content block order is invalid")
		}
		switch e.Block.Type {
		case "text", "thinking", "redacted_thinking", "tool_use":
		default:
			return errors.New("Claude returned an unsupported content block")
		}
		_, err := t.addBlock(e.Block)
		return err
	case "content_block_delta":
		if e.Index < 0 || e.Index >= len(t.blocks) || t.closed[e.Index] {
			return errors.New("Claude content block order is invalid")
		}
		var d struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			Partial   string `json:"partial_json"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}
		if json.Unmarshal(e.Delta, &d) != nil {
			return invalid()
		}
		b := &t.blocks[e.Index]
		switch d.Type {
		case "text_delta":
			if b.Type != "text" {
				return invalid()
			}
			b.Text += d.Text
		case "thinking_delta":
			if b.Type != "thinking" {
				return invalid()
			}
			b.Thinking += d.Thinking
		case "signature_delta":
			if b.Type != "thinking" {
				return invalid()
			}
			b.Signature += d.Signature
		case "input_json_delta":
			if b.Type != "tool_use" {
				return invalid()
			}
			t.partials[e.Index] += d.Partial
		default:
			return errors.New("Claude returned an unsupported stream delta")
		}
		t.bytes += len(e.Delta)
		if t.bytes > maxOutput {
			return errors.New("Claude response exceeded its limit")
		}
		return t.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": e.Index, "delta": json.RawMessage(e.Delta)})
	case "content_block_stop":
		return t.stopBlock(e.Index)
	case "message_delta":
		var d struct {
			Reason   string  `json:"stop_reason"`
			Sequence *string `json:"stop_sequence"`
		}
		if json.Unmarshal(e.Delta, &d) != nil {
			return invalid()
		}
		if d.Sequence != nil {
			return errors.New("Claude returned an unrequested stop sequence")
		}
		if d.Reason != "" {
			t.stopReason = d.Reason
		}
		return t.usageFrom(e.Usage)
	case "message_stop":
		for _, closed := range t.closed {
			if !closed {
				return errors.New("Claude ended an incomplete content block")
			}
		}
		if t.stopReason == "" {
			return errors.New("Claude stream ended without a confirmed stop reason")
		}
		t.completed = true
		for _, blocks := range t.transcriptBlocks {
			if err := t.validateTranscript(blocks); err != nil {
				return err
			}
		}
		t.transcriptBlocks = nil
		return nil
	case "ping":
		return nil
	case "error":
		return errors.New("Claude reported a generation error")
	default:
		return errors.New("Claude returned an unsupported stream event")
	}
}

func (t *turn) assistant(raw json.RawMessage) error {
	t.bytes += len(raw)
	if t.bytes > maxOutput {
		return errors.New("Claude response exceeded its limit")
	}
	t.transcript = true
	var m struct {
		ID         string          `json:"id"`
		Model      string          `json:"model"`
		Content    json.RawMessage `json:"content"`
		Usage      json.RawMessage `json:"usage"`
		StopReason string          `json:"stop_reason"`
	}
	if json.Unmarshal(raw, &m) != nil || m.ID == "" || len(m.ID) > 256 || m.Model == "<synthetic>" {
		return errors.New("Claude assistant message is invalid")
	}
	if t.nativeID != "" && m.ID != t.nativeID {
		return errors.New("Claude returned an unexpected second message")
	}
	t.nativeID = m.ID
	if err := t.usageFrom(m.Usage); err != nil {
		return err
	}
	blocks, err := content(m.Content, 0)
	if err != nil {
		return errors.New("Claude assistant content is unsupported")
	}
	if t.streamed {
		// Transcript and tap describe the same reply. In particular a tool call
		// cannot change its identity/name/arguments after the client saw it.
		if !t.completed {
			t.transcriptBlocks = append(t.transcriptBlocks, blocks)
			return nil
		}
		return t.validateTranscript(blocks)
	}
	if t.completed {
		return errors.New("Claude emitted content after a confirmed message boundary")
	}
	for _, b := range blocks {
		switch b.Type {
		case "text", "thinking", "redacted_thinking", "tool_use":
		default:
			return errors.New("Claude assistant content is unsupported")
		}
		i, err := t.addBlock(b)
		if err != nil {
			return err
		}
		if err = t.stopBlock(i); err != nil {
			return err
		}
	}
	if m.StopReason != "" {
		t.stopReason = m.StopReason
		t.completed = true
	}
	return nil
}

func (t *turn) validateTranscript(blocks []Block) error {
	for _, b := range blocks {
		matched := false
		if b.Type == "tool_use" {
			for _, c := range t.calls {
				if c.NativeID == b.ID && c.Alias == strings.TrimPrefix(b.Name, "mcp__magpie__") && string(canonical(c.Arguments)) == string(canonical(b.Input)) {
					matched = true
					break
				}
			}
		} else {
			for _, old := range t.blocks {
				if string(canonical(encode(blockValue(b)))) == string(canonical(encode(blockValue(old)))) {
					matched = true
					break
				}
			}
		}
		if !matched {
			return errors.New("Claude transcript does not match the emitted message")
		}
	}
	return nil
}

func (t *turn) finish(tool bool) error {
	if t.finished {
		return errors.New("Claude response was already completed")
	}
	if !t.completed {
		return errors.New("Claude response ended without a complete message")
	}
	reason := t.stopReason
	if tool {
		reason = "tool_use"
	} else {
		if len(t.calls) > 0 {
			return errors.New("Claude tool response lost its continuation")
		}
		if reason == "" {
			reason = "end_turn"
		}
		if reason != "end_turn" && reason != "max_tokens" {
			return errors.New("Claude returned an unsupported stop reason")
		}
	}
	if !t.started {
		if err := t.start(); err != nil {
			return err
		}
	}
	t.finished = true
	if !t.request.Stream {
		if err := t.sink.Head(200, "application/json"); err != nil {
			return err
		}
		data := encode(map[string]any{"id": t.id, "type": "message", "role": "assistant", "model": t.request.Model, "content": blockValues(t.blocks), "stop_reason": reason, "stop_sequence": nil, "usage": t.usage})
		if len(data) > maxOutput {
			return errors.New("Claude response exceeded its limit")
		}
		return t.sink.Write(data)
	}
	if err := t.event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": reason, "stop_sequence": nil}, "usage": t.usage}); err != nil {
		return err
	}
	return t.event("message_stop", map[string]any{"type": "message_stop"})
}

func blockValue(b Block) any {
	switch b.Type {
	case "text":
		return map[string]any{"type": b.Type, "text": b.Text}
	case "thinking":
		return map[string]any{"type": b.Type, "thinking": b.Thinking, "signature": b.Signature}
	case "redacted_thinking":
		return map[string]any{"type": b.Type, "data": b.Data}
	default:
		return b
	}
}
func blockValues(blocks []Block) []any {
	out := make([]any, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, blockValue(b))
	}
	return out
}
func (t *turn) assistantMessage() Message {
	return Message{Role: "assistant", Content: encode(blockValues(t.blocks))}
}
