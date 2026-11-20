package claudeprovider

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
)

type rpcRequest struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}
type fetchParams struct {
	Provider  string `json:"provider"`
	Account   string `json:"account"`
	Model     string `json:"model"`
	Method    string `json:"method"`
	Body      string `json:"body"`
	TimeoutMS int64  `json:"timeout_ms"`
	Window    int    `json:"window"`
}
type rpcWriter struct {
	out io.Writer
	mu  sync.Mutex
}

func (w *rpcWriter) send(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return json.NewEncoder(w.out).Encode(v)
}

type call struct {
	id          uint64
	ctx         context.Context
	cancel      context.CancelFunc
	writer      *rpcWriter
	mu          sync.Mutex
	credit      int
	wake        chan struct{}
	head        bool
	contentType string
}

func (c *call) Head(status int, contentType string) error {
	if c.head {
		return errors.New("provider response metadata was already sent")
	}
	c.head = true
	c.contentType = contentType
	return c.writer.send(map[string]any{"id": c.id, "event": "head", "status": status, "headers": map[string]string{"content-type": contentType, "cache-control": "no-store"}})
}
func (c *call) grant(n int) {
	if n <= 0 || n > 1<<20 {
		return
	}
	c.mu.Lock()
	c.credit = min(1<<20, c.credit+n)
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}
func (c *call) Write(b []byte) error {
	for len(b) > 0 {
		n := min(len(b), 16<<10)
		for {
			if err := c.ctx.Err(); err != nil {
				return err
			}
			c.mu.Lock()
			ready := c.credit >= n
			if ready {
				c.credit -= n
			}
			c.mu.Unlock()
			if ready {
				break
			}
			select {
			case <-c.ctx.Done():
				return c.ctx.Err()
			case <-c.wake:
			}
		}
		if err := c.writer.send(map[string]any{"id": c.id, "event": "chunk", "data": base64.StdEncoding.EncodeToString(b[:n])}); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

func (c *call) complete(err error) {
	if err != nil {
		status, code, message := 502, "provider_error", "Claude provider did not complete; verify the explicit native binding and retry only after checking the previous turn"
		var p *problem
		if errors.As(err, &p) {
			status, code, message = p.status, p.code, p.message
		}
		if errors.Is(err, context.DeadlineExceeded) {
			status, code, message = 504, "timeout", "Claude provider deadline expired"
		}
		if c.ctx.Err() != nil {
			return
		}
		payload := map[string]any{"type": "error", "error": map[string]string{"type": code, "message": message}}
		if !c.head {
			_ = c.Head(status, "application/json")
			_ = c.Write(encode(payload))
		} else if c.contentType == "text/event-stream" {
			_ = c.Write(append(append([]byte("event: error\ndata: "), encode(payload)...), []byte("\n\n")...))
		} else {
			_ = c.writer.send(map[string]any{"id": c.id, "error": map[string]string{"message": "Claude provider stream failed"}})
			return
		}
	}
	_ = c.writer.send(map[string]any{"id": c.id, "result": map[string]bool{"complete": true}})
}

// Serve is a lazy, parent-owned stdio worker, not a listening gateway. Credits
// bound unread output, abort cancels the exact call/parked tool capability,
// and stdin EOF cancels every owned native session before returning.
func Serve(root string, in io.Reader, out io.Writer) error {
	ctx, stop := workerContext()
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := &rpcWriter{out: out}
	m := newManager(ctx, root)
	var mu sync.Mutex
	calls := map[uint64]*call{}
	var jobs sync.WaitGroup
	defer func() { cancel(); m.close(); jobs.Wait() }()
	if closer, ok := in.(io.Closer); ok {
		go func() { <-ctx.Done(); _ = closer.Close() }()
	}
	s := bufio.NewScanner(in)
	s.Buffer(make([]byte, 64<<10), 12<<20)
	for s.Scan() {
		var r rpcRequest
		if unique(s.Bytes()) != nil || json.Unmarshal(s.Bytes(), &r) != nil {
			return errors.New("Claude provider RPC frame is invalid")
		}
		switch r.Method {
		case "credit", "abort":
			var p struct {
				ID uint64 `json:"id"`
				N  int    `json:"n"`
			}
			if json.Unmarshal(r.Params, &p) != nil {
				continue
			}
			mu.Lock()
			c := calls[p.ID]
			mu.Unlock()
			if r.Method == "credit" {
				if c != nil {
					c.grant(p.N)
				}
			} else {
				if c != nil {
					c.cancel()
				}
				m.abort(p.ID)
			}
		case "fetch":
			if r.ID == 0 {
				return errors.New("Claude provider call identity is missing")
			}
			var p fetchParams
			if json.Unmarshal(r.Params, &p) != nil || p.Provider != "builtin:claude" || p.Account == "" || len(p.Account) > 256 || p.Method != "POST" || p.TimeoutMS < 1 || p.TimeoutMS > 3600000 || p.Window < 16384 || p.Window > 1<<20 {
				_ = w.send(map[string]any{"id": r.ID, "error": map[string]string{"message": "Claude provider envelope is invalid"}})
				continue
			}
			body, err := base64.StdEncoding.Strict().DecodeString(p.Body)
			if err != nil || len(body) > maxRequest {
				_ = w.send(map[string]any{"id": r.ID, "error": map[string]string{"message": "Claude provider body exceeds its limit"}})
				continue
			}
			callCtx, callCancel := context.WithTimeout(ctx, time.Duration(min(p.TimeoutMS, 300000))*time.Millisecond)
			c := &call{id: r.ID, ctx: callCtx, cancel: callCancel, writer: w, credit: p.Window, wake: make(chan struct{}, 1)}
			mu.Lock()
			_, duplicate := calls[r.ID]
			if len(calls) >= maxSessions || duplicate {
				mu.Unlock()
				callCancel()
				_ = w.send(map[string]any{"id": r.ID, "error": map[string]string{"message": "Claude provider pending call limit reached"}})
				continue
			}
			calls[r.ID] = c
			mu.Unlock()
			jobs.Add(1)
			go func(p fetchParams, body []byte, c *call) {
				defer jobs.Done()
				defer c.cancel()
				defer func() { mu.Lock(); delete(calls, c.id); mu.Unlock() }()
				request, err := parseRequest(body, p.Model)
				if err == nil {
					err = m.fetch(c.ctx, c.id, p.Account, request, c)
				}
				c.complete(err)
			}(p, body, c)
		default:
			_ = w.send(map[string]any{"id": r.ID, "error": map[string]string{"message": "Claude provider RPC method is unsupported"}})
		}
	}
	return s.Err()
}
