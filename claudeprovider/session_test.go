package claudeprovider

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

func pendingSession(t *testing.T) (*manager, *session, *Request) {
	t.Helper()
	r := fixtureRequest(t)
	r.Tools = []Tool{{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	completedTurn := streamFixture(t, r, &memorySink{}, true)
	completedTurn.calls[0].attached = true
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := newManager(ctx, "")
	s := &session{manager: m, id: "fixture", account: "builtin:claude#0123456789abcdef0123", controls: r.controlFingerprint(), ctx: ctx, cancel: cancel, seenMessages: map[string]*turn{}}
	m.sessions[s.id] = s
	if err := m.park(1, s, completedTurn, r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s.timer != nil {
			s.timer.Stop()
		}
	})
	next := *r
	next.Messages = append(append([]Message(nil), r.Messages...), completedTurn.assistantMessage(), Message{Role: "user", Content: encode([]Block{{Type: "tool_result", ToolUseID: completedTurn.calls[0].ID, Content: json.RawMessage(`"done"`)}})})
	return m, s, &next
}

func TestContinuationBindsAccountModelToolsAndCompleteHistory(t *testing.T) {
	for _, change := range []string{"account", "model", "tools", "history", "assistant", "capability"} {
		t.Run(change, func(t *testing.T) {
			m, s, r := pendingSession(t)
			account := s.account
			switch change {
			case "account":
				account = "builtin:claude#99999999999999999999"
			case "model":
				r.Model = "opus"
			case "tools":
				r.Tools[0].InputSchema = json.RawMessage(`{"type":"object","required":["different"]}`)
			case "history":
				r.Messages[0].Content = json.RawMessage(`"other caller"`)
			case "assistant":
				r.Messages[1].Content = json.RawMessage(`[{"type":"text","text":"changed"}]`)
			case "capability":
				r.Messages[2].Content = encode([]Block{{Type: "tool_result", ToolUseID: "toolu_zc_" + string(make([]byte, 64)), Content: json.RawMessage(`"done"`)}})
			}
			results, _ := content(r.Messages[len(r.Messages)-1].Content, 0)
			if _, err := m.resume(2, account, r, results); err == nil {
				t.Fatal("accepted crossed continuation")
			}
			if len(m.capabilities) != 1 {
				t.Fatal("rejected continuation consumed original capability")
			}
		})
	}
}

func TestConcurrentResultsAreConsumedOnceAndOldAbortCannotCancelNextCall(t *testing.T) {
	m, s, r := pendingSession(t)
	results, _ := content(r.Messages[len(r.Messages)-1].Content, 0)
	var wg sync.WaitGroup
	accepted := make(chan uint64, 2)
	for _, id := range []uint64{2, 3} {
		wg.Add(1)
		go func(id uint64) {
			defer wg.Done()
			if _, err := m.resume(id, s.account, r, results); err == nil {
				accepted <- id
			}
		}(id)
	}
	wg.Wait()
	close(accepted)
	count := 0
	for range accepted {
		count++
	}
	if count != 1 {
		t.Fatalf("accepted %d duplicate result sets", count)
	}
	m.abort(1)
	if s.ctx.Err() != nil {
		t.Fatal("old call aborted resumed session")
	}
	if _, err := m.resume(4, s.account, r, results); err == nil {
		t.Fatal("replayed tool result")
	}
}

func TestUnconfirmedDrainRetainsTheSessionInsteadOfOnlyItsAccountFlag(t *testing.T) {
	m, s, _ := pendingSession(t)
	s.closeErr = errors.New("synthetic unconfirmed drain")
	m.forget(s)
	if !m.blocked[s.account] || len(m.sessions) != 0 || len(m.capabilities) != 0 || len(m.byCall) != 0 {
		t.Fatal("uncertain session remained callable")
	}
	if len(m.quarantined) != 1 || m.quarantined[0] != s {
		t.Fatal("lost the lease/handle owner before process exit was confirmed")
	}
}
