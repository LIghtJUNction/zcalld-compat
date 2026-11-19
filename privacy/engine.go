// Package privacy adapts the pinned Magpie engine without loading its gateway.
package privacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/internal/pii/guard"
	"github.com/LIghtJUNction/zcalld-compat/internal/redact"
)

const Limit = 16 << 20

type Config struct {
	Settings string `json:"settings"`
	Key      string `json:"key"`
	ModelDir string `json:"model_dir"`
}
type Settings struct {
	Enabled *bool `json:"zcalldEnabled"`

	ModelOff bool `json:"redactModelOff"`

	Secrets         bool               `json:"redact"`
	Personal        bool               `json:"redactPersonal"`
	Words           []string           `json:"redactWords"`
	WordsAnyCase    bool               `json:"redactWordsAnyCase"`
	WordsWhole      bool               `json:"redactWordsWhole"`
	Rules           []redact.Rule      `json:"redactRules"`
	Kinds           map[string]bool    `json:"redactKinds"`
	Actions         map[string]string  `json:"redactActions"`
	Allow           []string           `json:"redactAllow"`
	Shape           bool               `json:"redactShape"`
	Model           bool               `json:"redactModel"`
	ModelAuto       *bool              `json:"redactModelAuto"`
	ModelName       string             `json:"redactModelName"`
	ModelDir        string             `json:"redactModelDir"`
	ModelThresholds map[string]float64 `json:"redactModelScore"`
}
type Result struct {
	Enabled bool   `json:"enabled"`
	Body    []byte `json:"body"`
	Count   int    `json:"count,omitempty"`
	Blocked bool   `json:"blocked,omitempty"`
	Model   string `json:"model,omitempty"`
}
type Engine struct{ config Config }

var keyLock sync.Mutex
var keyPath string

func New(c Config) (*Engine, error) {
	if !filepath.IsAbs(c.Settings) || !filepath.IsAbs(c.Key) {
		return nil, errors.New("privacy settings and key paths must be absolute")
	}
	if _, err := readSettings(c.Settings); err != nil {
		return nil, err
	}
	keyLock.Lock()
	defer keyLock.Unlock()
	if keyPath != "" && keyPath != c.Key {
		return nil, errors.New("one privacy key path is allowed per worker")
	}
	// Require a durable private key; never silently replace an unreadable key.
	key, err := os.ReadFile(c.Key)
	if err != nil || len(key) < 32 {
		return nil, errors.New("privacy key is missing or invalid")
	}
	fi, err := os.Stat(c.Key)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0077 != 0 {
		return nil, errors.New("privacy key must be a private regular file")
	}
	redact.SetKeyPath(c.Key)
	keyPath = c.Key
	return &Engine{c}, nil
}
func readSettings(p string) (Settings, error) {
	var s Settings
	f, e := os.Open(p)
	if e != nil {
		return s, errors.New("privacy settings are unavailable")
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if e != nil || len(b) > 1<<20 {
		return s, errors.New("privacy settings exceed limit")
	}
	if json.Unmarshal(b, &s) != nil {
		return s, errors.New("privacy settings are invalid")
	}
	return s, nil
}
func (e *Engine) Mask(body []byte, route string) (r Result, err error) {
	defer func() {
		if recover() != nil {
			r = Result{}
			err = errors.New("privacy model failed; request held back")
		}
	}()
	if len(body) > Limit || !json.Valid(body) {
		return r, errors.New("invalid privacy request")
	}
	s, err := readSettings(e.config.Settings)
	if err != nil {
		return r, err
	}
	if s.Enabled != nil && !*s.Enabled {
		r.Body = body
		return r, nil
	}
	rules, err := redact.CheckRules(s.Rules)
	if err != nil {
		return r, errors.New("privacy custom rules are invalid")
	}
	o := redact.Options{Secrets: s.Secrets, Personal: s.Personal, Words: s.Words, WordsAnyCase: s.WordsAnyCase, WordsWhole: s.WordsWhole, Rules: rules, Kinds: s.Kinds, Actions: s.Actions, Allow: s.Allow, Shape: s.Shape, Route: route}
	o.Seen = func(h redact.Hit) {
		if h.Action == "block" {
			r.Blocked = true
		}
	}
	requested := s.Secrets || s.Personal || len(s.Words) > 0
	r.Enabled = requested
	if !requested {
		r.Body = body
		return r, nil
	}
	auto := !s.ModelOff && ((s.ModelAuto != nil && *s.ModelAuto) || (s.ModelAuto == nil && s.ModelName == ""))
	if s.Model || auto {
		name := s.ModelName
		if name == "" {
			name = "astrlink-guard"
		}
		if name != "astrlink-guard" {
			return r, fmt.Errorf("configured privacy model %q requires its local adapter; request held back", name)
		}
		dir := e.config.ModelDir
		if s.ModelDir != "" {
			dir = s.ModelDir
		}
		dir = guardDirectory(dir)
		m, merr := loadModel(dir)
		if merr != nil {
			if s.Model {
				return r, errors.New("explicit privacy model unavailable; request held back")
			}
			r.Model = "auto-rules-only"
		} else {
			threshold := guard.MinScore
			if n, ok := s.ModelThresholds[name]; ok {
				if n < guard.Floor || n > 1 {
					return r, errors.New("privacy threshold is invalid")
				}
				threshold = n
			}
			o.MinScore = threshold
			o.NamesByModel = true
			o.Model = func(texts []string) [][]redact.Found {
				modelState.Lock()
				defer modelState.Unlock()
				found := make([][]redact.Found, len(texts))
				for i, t := range texts {
					for _, v := range m.Find(t) {
						if v.Score >= threshold {
							found[i] = append(found[i], redact.Found{Start: v.Start, End: v.End, Label: label(v.Label), Score: v.Score})
						}
					}
				}
				modelState.last = time.Now()
				return found
			}
			r.Model = name
		}
	}
	r.Body, r.Count = redact.MaskJSON(body, o)
	if r.Blocked {
		r.Body = nil
		return r, nil
	}
	if len(r.Body) > Limit {
		return Result{}, errors.New("masked request exceeds limit")
	}
	return r, nil
}
func guardDirectory(root string) string {
	if _, err := os.Stat(filepath.Join(root, "model_int8.onnx")); err == nil {
		return root
	}
	return filepath.Join(root, "astrlink-guard", "7216b6a3b897")
}

var modelState = struct {
	sync.Mutex
	model *guard.Model
	dir   string
	last  time.Time
	timer *time.Timer
}{}

func loadModel(dir string) (*guard.Model, error) {
	modelState.Lock()
	defer modelState.Unlock()
	if modelState.model == nil || modelState.dir != dir {
		m, err := guard.Load(dir)
		if err != nil {
			return nil, err
		}
		modelState.model = m
		modelState.dir = dir
	}
	modelState.last = time.Now()
	if modelState.timer != nil {
		modelState.timer.Stop()
	}
	modelState.timer = time.AfterFunc(10*time.Minute, func() {
		modelState.Lock()
		defer modelState.Unlock()
		if time.Since(modelState.last) >= 10*time.Minute {
			modelState.model = nil
			runtime.GC()
		}
	})
	return modelState.model, nil
}
func label(s string) string {
	switch s {
	case "email":
		return "private_email"
	case "phone":
		return "private_phone"
	case "account", "payment_card":
		return "account_number"
	case "common_secret":
		return "secret"
	}
	return s
}

type sink struct {
	head http.Header
	bytes.Buffer
}

func (s *sink) Header() http.Header { return s.head }
func (s *sink) WriteHeader(int)     {}

type Stream struct {
	writer      *redact.Writer
	sink        *sink
	total       int
	closed      bool
	passthrough bool
}

func NewPassthroughStream() *Stream { return &Stream{passthrough: true} }

func NewStream(contentType string) *Stream {
	s := &sink{head: make(http.Header)}
	s.head.Set("Content-Type", contentType)
	w := redact.NewWriter(s)
	w.WriteHeader(200)
	return &Stream{writer: w, sink: s}
}
func (s *Stream) Write(body []byte, final bool) ([]byte, error) {
	if s.closed {
		return nil, errors.New("privacy stream is closed")
	}
	if s.passthrough {
		if final {
			s.closed = true
		}
		return append([]byte(nil), body...), nil
	}
	s.total += len(body)
	if s.total > Limit {
		s.closed = true
		return nil, errors.New("privacy response exceeds limit")
	}
	if _, e := s.writer.Write(body); e != nil {
		s.closed = true
		return nil, errors.New("privacy restoration failed")
	}
	if final {
		s.writer.Finish()
		s.closed = true
	}
	out := append([]byte(nil), s.sink.Bytes()...)
	s.sink.Reset()
	return out, nil
}
func Scrub(body []byte) []byte { return redact.ScrubJSON(body) }
