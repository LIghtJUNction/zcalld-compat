package maintenance

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

type manualHost struct {
	inspect func(context.Context, string) (Snapshot, error)
	warm    func(context.Context, string, []string) (ActionResult, error)
}

func (h manualHost) Inspect(c context.Context, a string) (Snapshot, error) { return h.inspect(c, a) }
func (h manualHost) Warmup(c context.Context, a string, w []string) (ActionResult, error) {
	return h.warm(c, a, w)
}
func (manualHost) Consume(context.Context, string, ConsumeRequest) (ActionResult, error) {
	panic("manual warmup consumed a reset card")
}

func TestManualWarmupThreeConcurrentAndLockCoversDrain(t *testing.T) {
	s := newStore(t, false, false)
	keys := []string{}
	for i := range 7 {
		keys = append(keys, fmt.Sprintf("builtin:claude#%020x", i))
	}
	entered := make(chan struct{}, len(keys))
	release := make(chan struct{})
	closing := make(chan struct{}, len(keys))
	drained := make(chan struct{})
	var active, maximum atomic.Int32
	e := Engine{Store: s, Now: func() time.Time { return moment }, ManualHost: func(string) (Host, func()) {
		h := manualHost{inspect: func(_ context.Context, a string) (Snapshot, error) { v := snap(moment); v.Account = a; return v, nil }, warm: func(context.Context, string, []string) (ActionResult, error) {
			n := active.Add(1)
			for {
				old := maximum.Load()
				if old >= n || maximum.CompareAndSwap(old, n) {
					break
				}
			}
			entered <- struct{}{}
			<-release
			active.Add(-1)
			return ActionResult{Status: "completed"}, nil
		}}
		return h, func() { closing <- struct{}{}; <-drained }
	}}
	done := make(chan error, 1)
	go func() {
		r, err := e.WarmAccounts(t.Context(), keys, true)
		if err == nil && len(r.Results) != len(keys) {
			err = fmt.Errorf("wrong completed count")
		}
		done <- err
	}()
	for range 3 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("first three actions did not run")
		}
	}
	select {
	case <-entered:
		t.Fatal("more than three sends")
	case <-time.After(40 * time.Millisecond):
	}
	close(release)
	select {
	case <-closing:
	case <-time.After(5 * time.Second):
		t.Fatal("host did not begin drain")
	}
	if tx, err := s.acquire(t.Context(), false); err != ErrBusy {
		if tx != nil {
			tx.close()
		}
		t.Fatal("ledger lock escaped host drain", err)
	}
	close(drained)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("manual warmup did not finish")
	}
	if maximum.Load() != 3 {
		t.Fatal(maximum.Load())
	}
}

func TestManualWarmupUnknownNotReplayedAndNoAutomaticEnable(t *testing.T) {
	s := newStore(t, false, false)
	if err := s.SetEnabled(t.Context(), false, moment); err != nil {
		t.Fatal(err)
	}
	var sends atomic.Int32
	h := manualHost{inspect: func(context.Context, string) (Snapshot, error) { return snap(moment), nil }, warm: func(context.Context, string, []string) (ActionResult, error) {
		sends.Add(1)
		return ActionResult{Status: "unknown"}, nil
	}}
	e := Engine{Store: s, Host: h, Now: func() time.Time { return moment }}
	first, err := e.WarmAccounts(t.Context(), []string{account}, false)
	if err != nil || first.Results[0].Status != "unknown" {
		t.Fatal(first, err)
	}
	second, err := e.WarmAccounts(t.Context(), []string{account}, false)
	if err != nil || second.Results[0].Status != "pending_review" || sends.Load() != 1 {
		t.Fatal(second, err)
	}
	d, err := s.Read()
	if err != nil || d.Policy.Enabled {
		t.Fatal("manual action enabled scheduled work", err)
	}
}

func TestManualAllSkipsRunningButSingleIsExplicit(t *testing.T) {
	s := newStore(t, false, false)
	var sends atomic.Int32
	h := manualHost{inspect: func(context.Context, string) (Snapshot, error) {
		v := snap(moment)
		v.Windows[0].Used = 4
		return v, nil
	}, warm: func(context.Context, string, []string) (ActionResult, error) {
		sends.Add(1)
		return ActionResult{Status: "completed"}, nil
	}}
	e := Engine{Store: s, Host: h, Now: func() time.Time { return moment }}
	r, err := e.WarmAccounts(t.Context(), []string{account}, true)
	if err != nil || r.Results[0].Status != "running" || sends.Load() != 0 {
		t.Fatal(r, err)
	}
	r, err = e.WarmAccounts(t.Context(), []string{account}, false)
	if err != nil || r.Results[0].Status != "completed" || sends.Load() != 1 {
		t.Fatal(r, err)
	}
}
