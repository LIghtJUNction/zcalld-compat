package routing

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

type Peer struct {
	Kind   string `json:"kind"`
	Local  string `json:"local,omitempty"`
	Remote string `json:"remote,omitempty"`
	PID    int    `json:"pid,omitempty"`
	UID    uint32 `json:"uid,omitempty"`
	Start  string `json:"start,omitempty"`
}
type Request struct {
	ID        uint64          `json:"id"`
	Op        string          `json:"op"`
	Config    json.RawMessage `json:"config,omitempty"`
	Group     string          `json:"group,omitempty"`
	Body      json.RawMessage `json:"body,omitempty"`
	Message   string          `json:"message,omitempty"`
	Agent     string          `json:"agent,omitempty"`
	Subagent  bool            `json:"subagent,omitempty"`
	Compact   bool            `json:"compact,omitempty"`
	Tokens    int64           `json:"tokens,omitempty"`
	At        int64           `json:"at,omitempty"`
	Workspace string          `json:"workspace,omitempty"`
	Peer      Peer            `json:"peer,omitempty"`
}
type answer struct {
	ID     uint64 `json:"id"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Validate is the one-shot configuration gate used by migration and check.
// Its only input is rule configuration, never authentication state.
func Validate(input io.Reader, output io.Writer) error {
	data, err := io.ReadAll(io.LimitReader(input, (8<<20)+1))
	if err != nil {
		return errors.New("cannot read routing configuration")
	}
	if _, err = New(data); err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(map[string]any{"ok": true, "version": 1})
}

// Serve is a bounded local stdio protocol. No request or directory is logged.
// EOF exits immediately; this worker never owns credentials or refreshes them.
func Serve(input io.Reader, output io.Writer) error {
	scan := bufio.NewScanner(input)
	scan.Buffer(make([]byte, 65536), 24<<20)
	out := bufio.NewWriterSize(output, 65536)
	enc := json.NewEncoder(out)
	var engine *Engine
	for scan.Scan() {
		var request Request
		response := answer{}
		if err := strictJSON(scan.Bytes(), &request); err != nil {
			response.Error = "invalid routing worker request"
		} else {
			response.ID = request.ID
			switch request.Op {
			case "configure":
				value, err := New(request.Config)
				if err != nil {
					response.Error = err.Error()
				} else {
					engine = value
					response.Result = map[string]any{"version": 1}
				}
			case "evaluate":
				if engine == nil {
					response.Error = "routing worker is not configured"
				} else {
					facts, err := request.facts()
					if err != nil {
						response.Error = err.Error()
					} else {
						if engine.needsFolder(request.Group) {
							facts.Folder = request.Workspace
							if facts.Folder == "" {
								facts.Folder = peerWorkspace(request.Peer)
							}
						}
						matches, err := engine.evaluate(request.Group, facts)
						if err != nil {
							response.Error = err.Error()
						} else {
							response.Result = map[string]any{"matches": matches}
						}
					}
				}
			case "cancel":
				// Calls are synchronous and bounded. A client can discard their result.
				response.Result = true
			default:
				response.Error = "unknown routing operation"
			}
		}
		if err := enc.Encode(response); err != nil {
			return err
		}
		if err := out.Flush(); err != nil {
			return err
		}
	}
	if scan.Err() != nil {
		return errors.New("routing input exceeded its limit")
	}
	return nil
}
func valueString(value any) string      { s, _ := value.(string); return s }
func valueMap(value any) map[string]any { m, _ := value.(map[string]any); return m }
func valueArray(value any) []any        { a, _ := value.([]any); return a }
func textParts(value any) []string {
	if s, ok := value.(string); ok {
		if s != "" {
			return []string{s}
		}
		return nil
	}
	var result []string
	for _, part := range valueArray(value) {
		item := valueMap(part)
		kind := valueString(item["type"])
		if kind == "" || kind == "text" || kind == "input_text" || kind == "output_text" {
			if text := valueString(item["text"]); text != "" {
				result = append(result, text)
			}
		}
	}
	return result
}
func messageParts(message map[string]any) []string {
	if parts, ok := message["parts"]; ok {
		return textParts(parts)
	}
	return textParts(message["content"])
}
func bodyTexts(body map[string]any) (system, last []string) {
	system = append(system, textParts(body["system"])...)
	system = append(system, textParts(body["instructions"])...)
	for _, key := range []string{"systemInstruction", "system_instruction"} {
		system = append(system, textParts(valueMap(body[key])["parts"])...)
	}
	for _, key := range []string{"messages", "input", "contents"} {
		messages := valueArray(body[key])
		for _, item := range messages {
			m := valueMap(item)
			if role := valueString(m["role"]); role == "system" || role == "developer" {
				system = append(system, messageParts(m)...)
			}
		}
		for i := len(messages) - 1; i >= 0; i-- {
			m := valueMap(messages[i])
			if valueString(m["role"]) == "user" {
				last = messageParts(m)
				return system, last
			}
		}
	}
	if s, ok := body["input"].(string); ok {
		last = []string{s}
	} else if s, ok := body["prompt"].(string); ok {
		last = []string{s}
	}
	return system, last
}
func imageIn(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		switch valueString(v["type"]) {
		case "image", "image_url", "input_image":
			return true
		}
		for _, key := range []string{"inlineData", "fileData", "inline_data", "file_data"} {
			m := valueMap(v[key])
			if strings.HasPrefix(valueString(m["mimeType"]), "image/") || strings.HasPrefix(valueString(m["mime_type"]), "image/") {
				return true
			}
		}
		for _, part := range v {
			if imageIn(part) {
				return true
			}
		}
	case []any:
		for _, part := range v {
			if imageIn(part) {
				return true
			}
		}
	}
	return false
}
func (request Request) facts() (Facts, error) {
	if len(request.Body) > 16<<20 || len(request.Message) > 256<<10 || len(request.Agent) > 512 ||
		len(request.Workspace) > 4096 || request.Tokens < 0 || request.At <= 0 {
		return Facts{}, errors.New("routing request exceeds its metadata bounds")
	}
	var body map[string]any
	if json.Unmarshal(request.Body, &body) != nil || body == nil {
		return Facts{}, errors.New("routing body must be an object")
	}
	q := Facts{Tokens: request.Tokens, Agent: request.Agent, Message: request.Message,
		Subagent: request.Subagent, Compact: request.Compact, At: time.Unix(request.At, 0)}
	q.System, q.Last = bodyTexts(body)
	for _, key := range []string{"messages", "input", "contents"} {
		q.Images = q.Images || imageIn(body[key])
	}
	levels := []any{body["reasoning_effort"], valueMap(body["reasoning"])["effort"], valueMap(body["output_config"])["effort"]}
	thinking := valueMap(body["thinking"])
	gemini := valueMap(valueMap(body["generationConfig"])["thinkingConfig"])
	if len(gemini) == 0 {
		gemini = valueMap(valueMap(body["generation_config"])["thinking_config"])
	}
	levels = append(levels, gemini["thinkingLevel"], gemini["thinking_level"])
	for _, value := range levels {
		if s, ok := value.(string); ok && s != "" {
			q.Effort = strings.ToLower(s)
			q.Off = q.Effort == "none" || q.Effort == "off" || q.Effort == "disabled"
			q.Thinking = !q.Off
			break
		}
	}
	switch valueString(thinking["type"]) {
	case "disabled":
		q.Off = true
		q.Thinking = false
	case "enabled", "adaptive":
		q.Thinking = true
	}
	for _, key := range []string{"thinkingBudget", "thinking_budget"} {
		if budget, ok := gemini[key].(float64); ok {
			q.Off = budget == 0
			q.Thinking = budget != 0
		}
	}
	if !q.Compact {
		text := strings.ToLower(q.Message)
		for _, needle := range []string{"summarize the conversation", "summary of the conversation", "context compaction", "compact this conversation", "总结以上对话", "总结当前对话"} {
			q.Compact = q.Compact || strings.Contains(text, needle)
		}
	}
	return q, nil
}
