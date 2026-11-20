package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestWorkerNativeAbortEOFAndDeadlineCancelSameOperation(t *testing.T) {
	for _, mode := range []string{"abort", "eof", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			input, parent := io.Pipe()
			defer parent.Close()
			var output bytes.Buffer
			started := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- ServeWorker(t.Context(), input, &output, func(ctx context.Context, a string, trigger WeeklyTrigger) (ActionResult, error) {
					if a != account || trigger != WeeklyRoutesExhausted {
						panic("worker changed request")
					}
					close(started)
					<-ctx.Done()
					return ActionResult{Status: "unknown", Code: "interrupted"}, ctx.Err()
				})
			}()
			timeout := 60000
			if mode == "deadline" {
				timeout = 30
			}
			fmt.Fprintf(parent, "{\"id\":7,\"method\":\"weekly\",\"params\":{\"account\":%q,\"trigger\":\"routes_exhausted\",\"timeout_ms\":%d}}\n", account, timeout)
			<-started
			switch mode {
			case "abort":
				fmt.Fprintln(parent, `{"method":"abort","params":{"id":7}}`)
			case "eof":
				parent.Close()
			}
			select {
			case e := <-done:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(time.Second):
				t.Fatal("owned operation ignored cancellation")
			}
			var reply struct {
				ID     uint64       `json:"id"`
				Result ActionResult `json:"result"`
			}
			if json.Unmarshal(output.Bytes(), &reply) != nil || reply.ID != 7 || reply.Result.Status != "unknown" {
				t.Fatal("unsafe cancellation result", output.String())
			}
		})
	}
}
func TestWorkerRejectsUnboundedOrUntrustedAuthority(t *testing.T) {
	valid := fmt.Sprintf(`{"id":1,"method":"weekly","params":{"account":%q,"trigger":"routes_exhausted","timeout_ms":5000}}`, account)
	for _, frame := range []string{strings.Repeat("x", workerFrameBytes+1), strings.Replace(valid, "weekly", "run-once", 1), strings.Replace(valid, "5000", "60001", 1), strings.Replace(valid, "routes_exhausted", "timer", 1), strings.Replace(valid, `"id":1`, `"id":0`, 1), strings.Replace(valid, `"timeout_ms":5000`, `"timeout_ms":5000,"url":"https://never.invalid/"`, 1)} {
		called := false
		e := ServeWorker(t.Context(), io.NopCloser(strings.NewReader(frame+"\n")), io.Discard, func(context.Context, string, WeeklyTrigger) (ActionResult, error) {
			called = true
			return ActionResult{Status: "completed", Code: "reset"}, nil
		})
		if e == nil || called {
			t.Fatal("invalid worker input reached reset logic")
		}
	}
}
