// Package claudeprovider is the optional native-Claude Messages adapter. It
// never reads OAuth secrets or makes vendor HTTP calls; the explicitly bound
// native Claude CLI owns those operations.
package claudeprovider

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxRequest = 8 << 20
const maxOutput = 32 << 20
const maxLine = 8 << 20
const maxSessions = 4
const maxTools = 64

type problem struct {
	status        int
	code, message string
}

func (e *problem) Error() string { return e.message }
func invalid() error             { return &problem{400, "invalid_request", "Claude provider request is invalid"} }
func unsupported() error {
	return &problem{400, "unsupported_request", "Claude provider does not support one of the requested fields or content types"}
}
func expired() error {
	return &problem{409, "tool_session_unavailable", "Tool continuation is unavailable; keep the complete conversation and start a new turn"}
}

type Request struct {
	Model        string          `json:"model"`
	MaxTokens    int             `json:"max_tokens"`
	Stream       bool            `json:"stream,omitempty"`
	System       json.RawMessage `json:"system,omitempty"`
	Messages     []Message       `json:"messages"`
	Tools        []Tool          `json:"tools,omitempty"`
	ToolChoice   *ToolChoice     `json:"tool_choice,omitempty"`
	OutputConfig *OutputConfig   `json:"output_config,omitempty"`
}
type OutputConfig struct {
	Effort string `json:"effort"`
}
type ToolChoice struct {
	Type string `json:"type"`
}
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}
type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Source    *ImageSource    `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
}

var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var plainTool = regexp.MustCompile(`^[A-Za-z0-9_-]{1,51}$`)
var hashSuffix = regexp.MustCompile(`_[0-9a-f]{12}$`)
var toolChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)
var capabilityID = regexp.MustCompile(`^toolu_zc_[0-9a-f]{64}$`)

func strict(raw []byte, out any) error {
	if !utf8.Valid(raw) || len(raw) > maxRequest {
		return invalid()
	}
	if err := unique(raw); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return unsupported()
	}
	var end any
	if d.Decode(&end) != io.EOF {
		return invalid()
	}
	return nil
}

// Reject duplicate keys before interpreting fields. A request cannot give
// policy/body controls two different meanings to separate JSON decoders.
func unique(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return invalid()
		}
		t, err := d.Token()
		if err != nil {
			return invalid()
		}
		if v, ok := t.(json.Delim); ok {
			switch v {
			case '{':
				keys := map[string]bool{}
				for d.More() {
					k, err := d.Token()
					if err != nil {
						return invalid()
					}
					s, ok := k.(string)
					if !ok || keys[s] {
						return invalid()
					}
					keys[s] = true
					if len(keys) > 65536 {
						return invalid()
					}
					if err = value(depth + 1); err != nil {
						return err
					}
				}
				end, err := d.Token()
				if err != nil || end != json.Delim('}') {
					return invalid()
				}
			case '[':
				for d.More() {
					if err = value(depth + 1); err != nil {
						return err
					}
				}
				end, err := d.Token()
				if err != nil || end != json.Delim(']') {
					return invalid()
				}
			default:
				return invalid()
			}
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return invalid()
	}
	return nil
}

func canonical(raw []byte) []byte {
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&v) != nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}
func encode(v any) []byte         { b, _ := json.Marshal(v); return b }
func fingerprint(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func parseRequest(raw []byte, model string) (*Request, error) {
	var r Request
	if err := strict(raw, &r); err != nil {
		return nil, err
	}
	if !modelName.MatchString(r.Model) || r.Model != model || r.MaxTokens < 1 || r.MaxTokens > 128000 || len(r.Messages) == 0 || len(r.Messages) > 1024 || len(r.Tools) > maxTools {
		return nil, invalid()
	}
	if r.Messages[len(r.Messages)-1].Role != "user" {
		return nil, unsupported()
	}
	if r.ToolChoice != nil && r.ToolChoice.Type != "auto" && r.ToolChoice.Type != "none" {
		return nil, unsupported()
	}
	if r.OutputConfig != nil {
		switch r.OutputConfig.Effort {
		case "low", "medium", "high":
		default:
			return nil, unsupported()
		}
	}
	if _, err := systemText(r.System); err != nil {
		return nil, err
	}
	if len(encode(r.Tools)) > 512<<10 {
		return nil, invalid()
	}
	seen := map[string]bool{}
	aliases := map[string]bool{}
	for _, t := range r.Tools {
		if t.Name == "" || len(t.Name) > 128 || strings.IndexFunc(t.Name, func(r rune) bool { return r < 32 || r == 127 }) >= 0 || seen[t.Name] || len(t.Description) > 64<<10 || len(t.InputSchema) > 128<<10 {
			return nil, invalid()
		}
		seen[t.Name] = true
		alias := bridgeName(t.Name)
		if aliases[alias] {
			return nil, invalid()
		}
		aliases[alias] = true
		var schema map[string]json.RawMessage
		if unique(t.InputSchema) != nil || json.Unmarshal(t.InputSchema, &schema) != nil || schema == nil {
			return nil, invalid()
		}
		var ty string
		_ = json.Unmarshal(schema["type"], &ty)
		if ty != "object" {
			return nil, unsupported()
		}
	}
	for _, m := range r.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return nil, invalid()
		}
		blocks, err := content(m.Content, 0)
		if err != nil {
			return nil, err
		}
		for _, b := range blocks {
			if b.Type == "tool_result" && m.Role != "user" || b.Type == "tool_use" && m.Role != "assistant" || ((b.Type == "thinking" || b.Type == "redacted_thinking") && m.Role != "assistant") {
				return nil, invalid()
			}
		}
	}
	return &r, nil
}

func content(raw json.RawMessage, depth int) ([]Block, error) {
	if depth > 2 || len(raw) == 0 {
		return nil, invalid()
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []Block{{Type: "text", Text: text}}, nil
	}
	var blocks []Block
	if err := strict(raw, &blocks); err != nil {
		return nil, err
	}
	if len(blocks) > 4096 {
		return nil, invalid()
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Source != nil || b.ID != "" || b.Name != "" || len(b.Input) != 0 || b.ToolUseID != "" || len(b.Content) != 0 || b.IsError || b.Thinking != "" || b.Signature != "" || b.Data != "" {
				return nil, invalid()
			}
		case "image":
			if b.Source == nil || b.Source.Type != "base64" {
				return nil, unsupported()
			}
			s := b.Source
			if s.MediaType != "image/png" && s.MediaType != "image/jpeg" && s.MediaType != "image/gif" && s.MediaType != "image/webp" {
				return nil, unsupported()
			}
			if len(s.Data) > 6<<20 {
				return nil, invalid()
			}
			data, err := base64.StdEncoding.Strict().DecodeString(s.Data)
			if err != nil || len(data) == 0 || len(data) > 4<<20 {
				return nil, invalid()
			}
			if b.Text != "" || b.ID != "" || b.Name != "" || len(b.Input) != 0 || b.ToolUseID != "" || len(b.Content) != 0 || b.Thinking != "" || b.Data != "" || b.Signature != "" || b.IsError {
				return nil, invalid()
			}
		case "tool_use":
			if b.ID == "" || len(b.ID) > 128 || b.Name == "" || len(b.Name) > 128 || b.Text != "" || b.Source != nil || b.ToolUseID != "" || len(b.Content) != 0 || b.Thinking != "" || b.Signature != "" || b.Data != "" || b.IsError {
				return nil, invalid()
			}
			var args map[string]json.RawMessage
			if unique(b.Input) != nil || json.Unmarshal(b.Input, &args) != nil || args == nil {
				return nil, invalid()
			}
		case "tool_result":
			if depth != 0 || b.ToolUseID == "" || len(b.ToolUseID) > 128 || b.Source != nil || b.ID != "" || b.Name != "" || len(b.Input) != 0 || b.Text != "" || b.Thinking != "" || b.Signature != "" || b.Data != "" {
				return nil, invalid()
			}
			parts, err := content(b.Content, depth+1)
			if err != nil {
				return nil, err
			}
			for _, p := range parts {
				if p.Type != "text" && p.Type != "image" {
					return nil, unsupported()
				}
			}
		case "thinking":
			if b.Source != nil || b.ID != "" || b.Name != "" || len(b.Input) != 0 || b.ToolUseID != "" || len(b.Content) != 0 || b.Text != "" || b.Data != "" || b.IsError {
				return nil, invalid()
			}
		case "redacted_thinking":
			if b.Source != nil || b.ID != "" || b.Name != "" || len(b.Input) != 0 || b.ToolUseID != "" || len(b.Content) != 0 || b.Text != "" || b.Thinking != "" || b.Signature != "" || b.IsError {
				return nil, invalid()
			}
		default:
			return nil, unsupported()
		}
	}
	return blocks, nil
}

func systemText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	blocks, err := content(raw, 1)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, b := range blocks {
		if b.Type != "text" {
			return "", unsupported()
		}
		parts = append(parts, b.Text)
	}
	text := strings.Join(parts, "\n\n")
	if len(text) > 256<<10 {
		return "", invalid()
	}
	return text, nil
}

// The deterministic name mapping is adapted from the pinned Magpie
// claude_subscription.go bridgeToolName. It avoids Claude normalizing two
// different client names to the same MCP tool.
func bridgeName(name string) string {
	if rest, ok := strings.CutPrefix(name, "mcp__magpie__"); ok && plainTool.MatchString(rest) && !hashSuffix.MatchString(rest) {
		return rest
	}
	if plainTool.MatchString(name) && !hashSuffix.MatchString(name) {
		return name
	}
	h := sha256.Sum256([]byte(name))
	short := toolChars.ReplaceAllString(name, "_")
	if len(short) > 38 {
		short = short[:38]
	}
	return short + "_" + hex.EncodeToString(h[:6])
}

func (r *Request) controlFingerprint() string {
	copy := *r
	copy.Messages = nil
	copy.Stream = false
	return fingerprint(encode(copy))
}

// New CLI sessions render complete, validated conversation content. Opaque
// reasoning is only accepted when resuming this process's own pending turn;
// it is never converted to plain text and replayed to a new native session.
func (r *Request) prompt() ([]Block, error) {
	var out []Block
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			out = append(out, Block{Type: "text", Text: text.String()})
			text.Reset()
		}
	}
	for i, m := range r.Messages {
		if len(r.Messages) > 1 {
			if i == 0 {
				text.WriteString("<conversation_history>\n")
			}
			if i == len(r.Messages)-1 {
				text.WriteString("</conversation_history>\n<current_turn>\n")
			}
			if m.Role == "assistant" {
				text.WriteString("Assistant: ")
			} else {
				text.WriteString("Human: ")
			}
		}
		blocks, _ := content(m.Content, 0)
		for _, b := range blocks {
			switch b.Type {
			case "text":
				text.WriteString(b.Text)
			case "image":
				flush()
				out = append(out, b)
			case "tool_use":
				text.WriteString("\n[tool call " + "mcp__magpie__" + bridgeName(b.Name) + " id=" + b.ID + " args=" + string(canonical(b.Input)) + "]\n")
			case "tool_result":
				text.WriteString("\n[tool result id=" + b.ToolUseID)
				if b.IsError {
					text.WriteString(" is_error=true")
				}
				text.WriteString("]\n")
				parts, _ := content(b.Content, 1)
				for _, p := range parts {
					if p.Type == "text" {
						text.WriteString(p.Text)
					} else {
						flush()
						out = append(out, p)
					}
				}
				text.WriteString("\n[end of tool result]\n")
			case "thinking", "redacted_thinking":
				return nil, unsupported()
			}
		}
		text.WriteString("\n\n")
	}
	if len(r.Messages) > 1 {
		text.WriteString("</current_turn>")
	}
	flush()
	return out, nil
}
