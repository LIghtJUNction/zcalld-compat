package claudeprovider

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/claudemaintenance"
	"github.com/LIghtJUNction/zcalld-compat/internal/claudebridge"
)

type manager struct {
	ctx          context.Context
	root         string
	mu           sync.Mutex
	sessions     map[string]*session
	capabilities map[string]*session
	byCall       map[uint64]*session
	blocked      map[string]bool
	// Retain the actual lease/handles after an unconfirmed drain. Keeping only
	// a boolean would allow the File finalizer to release the native marker.
	quarantined []*session
}

type session struct {
	manager               *manager
	id, account, controls string
	ctx                   context.Context
	cancel                context.CancelFunc
	initialized           chan struct{}
	lease                 *claudemaintenance.ProviderLease
	process               *claudemaintenance.ProviderProcess
	server                *http.Server
	listener              net.Listener
	lines                 chan []byte
	callbacks             chan *callback
	previous              *turn
	history               []Message
	seenMessages          map[string]*turn
	turns, retained       int
	busy                  bool
	timer                 *time.Timer
	once                  sync.Once
	closeErr              error
}

type callback struct {
	ID        string          `json:"tool_call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	answer    chan toolAnswer
	accepted  chan bool
}

func newManager(ctx context.Context, root string) *manager {
	return &manager{ctx: ctx, root: root, sessions: map[string]*session{}, capabilities: map[string]*session{}, byCall: map[uint64]*session{}, blocked: map[string]bool{}}
}

func (m *manager) abort(id uint64) {
	m.mu.Lock()
	s := m.byCall[id]
	m.mu.Unlock()
	if s != nil {
		s.cancel()
	}
}
func (m *manager) forget(s *session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, s.id)
	for key, v := range m.capabilities {
		if v == s {
			delete(m.capabilities, key)
		}
	}
	for id, v := range m.byCall {
		if v == s {
			delete(m.byCall, id)
		}
	}
	if s.closeErr != nil {
		m.blocked[s.account] = true
		m.quarantined = append(m.quarantined, s)
	}
}
func (m *manager) close() {
	m.mu.Lock()
	all := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
		s.cancel()
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range all {
		wg.Add(1)
		go func(s *session) { defer wg.Done(); _ = s.close() }(s)
	}
	wg.Wait()
}

func (m *manager) newSession(ctx context.Context, id uint64, account string, r *Request) (*session, error) {
	if !strings.HasPrefix(account, "builtin:claude#") {
		return nil, &problem{400, "account_required", "Select an explicit native Claude account"}
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 8*time.Second {
		return nil, context.DeadlineExceeded
	}
	sid, err := randomID("session_")
	if err != nil {
		return nil, err
	}
	owned, cancel := context.WithDeadline(m.ctx, deadline)
	s := &session{manager: m, id: sid, account: account, controls: r.controlFingerprint(), ctx: owned, cancel: cancel, initialized: make(chan struct{}), lines: make(chan []byte, 2), callbacks: make(chan *callback, maxTools), seenMessages: map[string]*turn{}, busy: true}
	m.mu.Lock()
	if m.blocked[account] {
		m.mu.Unlock()
		cancel()
		return nil, &problem{503, "previous_process_unknown", "A previous native Claude process did not confirm shutdown; verify it before restarting the optional worker"}
	}
	if len(m.sessions)+len(m.quarantined) >= maxSessions {
		m.mu.Unlock()
		cancel()
		return nil, &problem{429, "session_limit", "Complete or verify shutdown of an existing Claude turn before starting another"}
	}
	m.sessions[sid] = s
	m.byCall[id] = s
	m.mu.Unlock()
	go func() { <-owned.Done(); _ = s.close() }()
	err = s.initialize(r)
	if err != nil {
		_ = s.close()
		return nil, err
	}
	return s, nil
}

func (s *session) initialize(r *Request) (err error) {
	defer close(s.initialized)
	s.lease, err = (claudemaintenance.Host{Root: s.manager.root}).OpenProvider(s.ctx, s.account, r.Model)
	if err != nil {
		if errors.Is(err, claudemaintenance.ErrProviderHeld) {
			return &problem{429, "usage_held", "Claude account usage limit is reached"}
		}
		return err
	}
	tools := []claudebridge.Tool{}
	if r.ToolChoice == nil || r.ToolChoice.Type != "none" {
		for _, t := range r.Tools {
			tools = append(tools, claudebridge.Tool{Name: bridgeName(t.Name), Description: t.Description, InputSchema: t.InputSchema})
		}
	}
	mcp := map[string]any{"mcpServers": map[string]any{}}
	if len(tools) > 0 {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return errors.New("Claude tool callback could not start")
		}
		s.listener = listener
		token, err := randomID("")
		if err != nil {
			return err
		}
		path := "/tool/" + token
		callbackFile, err := s.lease.Write("callback", []byte("http://"+listener.Addr().String()+path))
		if err != nil {
			return err
		}
		toolsFile, err := s.lease.Write("tools.json", encode(tools))
		if err != nil {
			return err
		}
		listed := filepath.Join(s.lease.Directory(), "tools-listed")
		mcp["mcpServers"] = map[string]any{"magpie": map[string]any{"command": s.lease.Helper(), "args": []string{"claude-mcp-helper", callbackFile, toolsFile, listed}}}
		s.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.toolHTTP(path, w, r) }), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 130 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
		go func() { _ = s.server.Serve(listener) }()
	}
	system, err := systemText(r.System)
	if err != nil {
		return err
	}
	systemFile := ""
	if system != "" {
		systemFile, err = s.lease.Write("system.txt", []byte(system))
		if err != nil {
			return err
		}
	}
	effort := ""
	if r.OutputConfig != nil {
		effort = r.OutputConfig.Effort
	}
	s.process, err = s.lease.Start(s.ctx, claudemaintenance.ProviderLaunch{MCPConfig: string(encode(mcp)), SystemFile: systemFile, MaxTokens: r.MaxTokens, Effort: effort})
	if err != nil {
		return err
	}
	go func() {
		defer close(s.lines)
		scanner := bufio.NewScanner(s.process.Output)
		scanner.Buffer(make([]byte, 64<<10), maxLine)
		for scanner.Scan() {
			b := append([]byte(nil), scanner.Bytes()...)
			select {
			case s.lines <- b:
			case <-s.ctx.Done():
				return
			}
		}
	}()
	return nil
}

func (s *session) close() error {
	s.cancel()
	<-s.initialized
	s.once.Do(func() {
		s.manager.mu.Lock()
		if s.timer != nil {
			s.timer.Stop()
			s.timer = nil
		}
		s.manager.mu.Unlock()
		if s.server != nil {
			_ = s.server.Close()
		} else if s.listener != nil {
			_ = s.listener.Close()
		}
		if s.process != nil {
			s.closeErr = s.process.Close()
		}
		if s.lease != nil && s.closeErr == nil {
			s.closeErr = s.lease.Close()
		}
		s.manager.forget(s)
	})
	return s.closeErr
}

func (s *session) toolHTTP(path string, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != "POST" || subtle.ConstantTimeCompare([]byte(r.URL.Path), []byte(path)) != 1 || r.URL.RawQuery != "" || r.Header.Get("Origin") != "" {
		http.Error(w, "Rejected", 403)
		return
	}
	defer r.Body.Close()
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequest))
	if err != nil {
		http.Error(w, "Rejected", 413)
		return
	}
	var cb callback
	if strict(data, &cb) != nil || cb.ID == "" || len(cb.ID) > 256 || cb.Name == "" {
		http.Error(w, "Rejected", 400)
		return
	}
	cb.answer = make(chan toolAnswer, 1)
	cb.accepted = make(chan bool, 1)
	select {
	case s.callbacks <- &cb:
	case <-s.ctx.Done():
		http.Error(w, "Session ended", 410)
		return
	case <-r.Context().Done():
		return
	}
	select {
	case ok := <-cb.accepted:
		if !ok {
			http.Error(w, "Rejected", 409)
			return
		}
	case <-s.ctx.Done():
		http.Error(w, "Session ended", 410)
		return
	case <-r.Context().Done():
		return
	}
	select {
	case result := <-cb.answer:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	case <-s.ctx.Done():
		http.Error(w, "Session ended", 410)
	case <-r.Context().Done():
		s.cancel()
	}
}

func messageDigest(m Message) string {
	blocks, err := content(m.Content, 0)
	if err != nil {
		return ""
	}
	for i := range blocks {
		if len(blocks[i].Input) > 0 {
			blocks[i].Input = canonical(blocks[i].Input)
		}
		if blocks[i].Type == "tool_result" {
			parts, _ := content(blocks[i].Content, 1)
			blocks[i].Content = encode(blockValues(parts))
		}
	}
	return fingerprint(encode(struct {
		Role    string
		Content []any
	}{m.Role, blockValues(blocks)}))
}

func (m *manager) resume(id uint64, account string, r *Request, results []Block) (*session, error) {
	if len(results) == 0 {
		return nil, expired()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.capabilities[results[0].ToolUseID]
	if s == nil || s.busy || s.account != account || s.controls != r.controlFingerprint() || s.ctx.Err() != nil || s.previous == nil {
		return nil, expired()
	}
	if len(r.Messages) != len(s.history)+2 {
		return nil, expired()
	}
	for i, h := range s.history {
		if messageDigest(h) != messageDigest(r.Messages[i]) {
			return nil, expired()
		}
	}
	if messageDigest(s.previous.assistantMessage()) != messageDigest(r.Messages[len(s.history)]) {
		return nil, expired()
	}
	if len(results) != len(s.previous.calls) {
		return nil, expired()
	}
	seen := map[string]bool{}
	for _, b := range results {
		if b.Type != "tool_result" || !capabilityID.MatchString(b.ToolUseID) || seen[b.ToolUseID] || m.capabilities[b.ToolUseID] != s {
			return nil, expired()
		}
		seen[b.ToolUseID] = true
	}
	for _, c := range s.previous.calls {
		if !seen[c.ID] {
			return nil, expired()
		}
	}
	// Consume the whole set atomically. Replayed/parallel results cannot both
	// release a blocked native tool, and an old abort cannot cancel its successor.
	for _, c := range s.previous.calls {
		delete(m.capabilities, c.ID)
	}
	for callID, current := range m.byCall {
		if current == s {
			delete(m.byCall, callID)
		}
	}
	s.busy = true
	m.byCall[id] = s
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	return s, nil
}

func (m *manager) park(id uint64, s *session, t *turn, r *Request) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	s.turns++
	s.retained += len(encode(t.blocks))
	if s.turns > 16 || s.retained > maxOutput {
		return &problem{409, "session_limit", "Claude tool session reached its context limit; begin a new turn with the complete conversation"}
	}
	s.previous = t
	s.history = append([]Message(nil), r.Messages...)
	s.busy = false
	s.seenMessages[t.nativeID] = t
	for _, c := range t.calls {
		if !c.attached {
			return errors.New("Claude tool callback is missing")
		}
		m.capabilities[c.ID] = s
	}
	m.byCall[id] = s
	s.timer = time.AfterFunc(120*time.Second, s.cancel)
	return nil
}

func (m *manager) fetch(ctx context.Context, id uint64, account string, r *Request, sink responseSink) (err error) {
	last, _ := content(r.Messages[len(r.Messages)-1].Content, 0)
	continuation := false
	for _, b := range last {
		if b.Type == "tool_result" && strings.HasPrefix(b.ToolUseID, "toolu_zc_") {
			continuation = true
		}
	}
	var s *session
	if continuation {
		for _, b := range last {
			if b.Type != "tool_result" {
				return unsupported()
			}
		}
		s, err = m.resume(id, account, r, last)
	} else {
		if _, err = r.prompt(); err != nil {
			return err
		}
		s, err = m.newSession(ctx, id, account, r)
	}
	if err != nil {
		return err
	}
	parked := false
	defer func() {
		if !parked {
			if closeErr := s.close(); closeErr != nil {
				err = closeErr
			}
		}
	}()
	t, err := newTurn(r, sink)
	if err != nil {
		return err
	}
	if continuation {
		deadline, _ := s.ctx.Deadline()
		checkCtx, cancelCheck := context.WithDeadline(ctx, deadline)
		stopCheck := context.AfterFunc(s.ctx, cancelCheck)
		err = s.lease.Check(checkCtx)
		stopCheck()
		cancelCheck()
		if err != nil {
			if errors.Is(err, claudemaintenance.ErrProviderHeld) {
				return &problem{429, "usage_held", "Claude account usage limit is reached"}
			}
			return err
		}
		if err = s.ctx.Err(); err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = s.lease.Current(); err != nil {
			return err
		}
		for _, result := range last {
			for _, c := range s.previous.calls {
				if c.ID == result.ToolUseID {
					parts, _ := content(result.Content, 1)
					c.response <- toolAnswer{Content: parts, IsError: result.IsError}
				}
			}
		}
	} else {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-s.ctx.Done():
				return s.ctx.Err()
			case line, ok := <-s.lines:
				if !ok {
					return errors.New("Claude interactive helper ended before ready")
				}
				var init struct {
					Type    string `json:"type"`
					Subtype string `json:"subtype"`
				}
				if json.Unmarshal(line, &init) != nil {
					return errors.New("Claude helper output is invalid")
				}
				if init.Type == "system" && init.Subtype == "init" {
					goto ready
				}
				if init.Type == "result" {
					return errors.New("Claude interactive helper did not become ready")
				}
			}
		}
	ready:
		if err = s.ctx.Err(); err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = s.lease.Current(); err != nil {
			return err
		}
		prompt, parseErr := r.prompt()
		if parseErr != nil {
			return parseErr
		}
		if err = s.process.Send(encode(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": blockValues(prompt)}})); err != nil {
			return err
		}
	}
	pending := map[string]*callback{}
	attach := func() error {
		for _, c := range t.calls {
			if c.attached {
				continue
			}
			cb := pending[c.NativeID]
			if cb == nil {
				continue
			}
			// Arguments are only complete at content_block_stop / transcript.
			if !t.completed {
				continue
			}
			if cb.Name != c.Alias || string(canonical(cb.Arguments)) != string(canonical(c.Arguments)) {
				cb.accepted <- false
				return errors.New("Claude tool callback does not match its message")
			}
			c.attached = true
			delete(pending, c.NativeID)
			cb.accepted <- true
			go func(c *toolCall, cb *callback) {
				select {
				case value := <-c.response:
					select {
					case cb.answer <- value:
					case <-s.ctx.Done():
					}
				case <-s.ctx.Done():
				}
			}(c, cb)
		}
		if t.completed {
			for id, cb := range pending {
				known := false
				for _, c := range t.calls {
					known = known || c.NativeID == id
				}
				if !known {
					cb.accepted <- false
					return errors.New("Claude tool callback has no matching message")
				}
			}
		}
		return nil
	}
	for {
		if err = attach(); err != nil {
			return err
		}
		if t.completed && len(t.calls) > 0 {
			all := len(pending) == 0
			for _, c := range t.calls {
				all = all && c.attached
			}
			if all {
				if err = m.park(id, s, t, r); err != nil {
					return err
				}
				if err = t.finish(true); err != nil {
					return err
				}
				parked = true
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.ctx.Done():
			return s.ctx.Err()
		case cb := <-s.callbacks:
			if pending[cb.ID] != nil || len(pending) >= maxTools {
				cb.accepted <- false
				return errors.New("Claude duplicated a tool callback")
			}
			for _, c := range t.calls {
				if c.NativeID == cb.ID && c.attached {
					cb.accepted <- false
					return errors.New("Claude duplicated a tool callback")
				}
			}
			pending[cb.ID] = cb
		case line, ok := <-s.lines:
			if !ok {
				return errors.New("Claude interactive helper ended before a result")
			}
			var v struct {
				Type     string          `json:"type"`
				Event    json.RawMessage `json:"event"`
				Message  json.RawMessage `json:"message"`
				IsError  bool            `json:"is_error"`
				Usage    json.RawMessage `json:"usage"`
				APIError bool            `json:"isApiErrorMessage"`
			}
			if json.Unmarshal(line, &v) != nil {
				return errors.New("Claude helper output is invalid")
			}
			switch v.Type {
			case "stream_event":
				if err = t.stream(v.Event); err != nil {
					return err
				}
			case "assistant":
				if v.APIError {
					return errors.New("Claude reported a provider error")
				}
				var identity struct {
					ID      string          `json:"id"`
					Content json.RawMessage `json:"content"`
				}
				if json.Unmarshal(v.Message, &identity) != nil {
					return invalid()
				}
				if old := s.seenMessages[identity.ID]; old != nil {
					blocks, parseErr := content(identity.Content, 0)
					if parseErr != nil {
						return parseErr
					}
					if err = old.validateTranscript(blocks); err != nil {
						return err
					}
					continue
				}
				if err = t.assistant(v.Message); err != nil {
					return err
				}
			case "result":
				if v.IsError {
					return errors.New("Claude generation did not complete")
				}
				if t.nativeID == "" || len(t.blocks) == 0 {
					return errors.New("Claude generation returned no assistant message")
				}
				if err = t.usageFrom(v.Usage); err != nil {
					return err
				}
				if len(pending) > 0 {
					return errors.New("Claude tool callback has no matching message")
				}
				if err = s.lease.Current(); err != nil {
					return err
				}
				if !t.streamed && len(t.calls) == 0 {
					t.completed = true
				}
				if err = s.close(); err != nil {
					return err
				}
				return t.finish(false)
			case "system", "user", "rate_limit_event", "control_response", "command_lifecycle":
				// Helper lifecycle records are not model output or routing input.
			default:
				return errors.New("Claude returned an unsupported helper event")
			}
		}
	}
}
