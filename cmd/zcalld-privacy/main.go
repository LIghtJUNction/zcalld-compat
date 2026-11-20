// zcalld-privacy is an optional isolated worker. It never opens a socket.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/LIghtJUNction/zcalld-compat/internal/buildinfo"
	"github.com/LIghtJUNction/zcalld-compat/privacy"
	"io"
	"os"
)

type request struct {
	ID          uint64          `json:"id"`
	Op          string          `json:"op"`
	Config      privacy.Config  `json:"config"`
	Body        []byte          `json:"body"`
	Route       string          `json:"route"`
	Context     privacy.Context `json:"context"`
	Stream      uint64          `json:"stream"`
	ContentType string          `json:"content_type"`
	Enabled     bool            `json:"enabled"`
	Final       bool            `json:"final"`
}
type answer struct {
	ID     uint64 `json:"id"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(buildinfo.String("zcalld-privacy"))
		return
	}
	os.Exit(run(os.Stdin, os.Stdout, os.Stderr))
}

func run(input io.Reader, output io.Writer, diagnostic io.Writer) (code int) {
	scan := bufio.NewScanner(input)
	scan.Buffer(make([]byte, 65536), 24<<20)
	out := bufio.NewWriterSize(output, 65536)
	enc := json.NewEncoder(out)
	var engine *privacy.Engine
	auditWarned := false
	defer func() {
		if engine != nil {
			if err := engine.Close(); err != nil {
				fmt.Fprintln(diagnostic, "privacy audit counter flush failed")
				code = 1
			}
		}
	}()
	streams := map[uint64]*privacy.Stream{}
	for scan.Scan() {
		var r request
		a := answer{}
		if json.Unmarshal(scan.Bytes(), &r) != nil {
			a.Error = "invalid worker request"
		} else {
			a.ID = r.ID
			switch r.Op {
			case "configure":
				e, err := privacy.New(r.Config)
				if err != nil {
					a.Error = err.Error()
				} else {
					if engine != nil {
						if err := engine.Close(); err != nil {
							fmt.Fprintln(diagnostic, "privacy audit counter flush failed")
						}
					}
					engine = e
					a.Result = true
				}
			case "mask":
				if engine == nil {
					a.Error = "privacy worker is not configured"
				} else {
					v, err := engine.MaskWithContext(r.Body, r.Route, r.Context)
					if err != nil {
						a.Error = err.Error()
					} else {
						a.Result = v
						if v.AuditError != "" && !auditWarned {
							fmt.Fprintln(diagnostic, "privacy audit history write failed; redaction remains active")
						}
						auditWarned = v.AuditError != ""
					}
				}
			case "flush":
				if engine == nil {
					a.Error = "privacy worker is not configured"
				} else if err := engine.Flush(); err != nil {
					a.Error = err.Error()
				} else {
					a.Result = true
				}
			case "open":
				if engine == nil {
					a.Error = "privacy worker is not configured"
				} else if len(streams) >= 128 {
					a.Error = "too many privacy streams"
				} else if _, ok := streams[r.Stream]; ok {
					a.Error = "duplicate privacy stream"
				} else {
					if r.Enabled {
						streams[r.Stream] = privacy.NewStream(r.ContentType)
					} else {
						streams[r.Stream] = privacy.NewPassthroughStream()
					}
					a.Result = true
				}
			case "write":
				s, ok := streams[r.Stream]
				if !ok {
					a.Error = "unknown privacy stream"
				} else {
					b, err := s.Write(r.Body, r.Final)
					if err != nil {
						a.Error = err.Error()
						delete(streams, r.Stream)
					} else {
						a.Result = map[string]any{"body": b}
						if r.Final {
							delete(streams, r.Stream)
						}
					}
				}
			case "close":
				delete(streams, r.Stream)
				a.Result = true
			case "scrub":
				a.Result = map[string]any{"body": privacy.Scrub(r.Body)}
			default:
				a.Error = "unknown privacy operation"
			}
		}
		if enc.Encode(a) != nil || out.Flush() != nil {
			return 1
		}
	}
	if scan.Err() != nil {
		fmt.Fprintln(diagnostic, "privacy input exceeded limit")
		return 1
	}
	return 0
}
