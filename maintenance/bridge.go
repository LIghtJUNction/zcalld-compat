package maintenance

import (
	"context"
	"encoding/json"
	"errors"
)

// Caller is the trusted host adapter. It must use the supplied deadline as a
// total bound, reserving time for existing credential renewal checkpoint/drain.
type Caller func(context.Context, string, map[string]any) (json.RawMessage, error)
type RPC struct{ Call Caller }

func (r RPC) request(ctx context.Context, method, account string, params map[string]any, out any) error {
	if r.Call == nil || !accountPattern.MatchString(account) {
		return ErrInvalid
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("maintenance host requires an absolute deadline")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	params["account"] = account
	params["deadline_ms"] = deadline.UnixMilli()
	b, err := r.Call(ctx, method, params)
	if err != nil {
		return err
	}
	if len(b) > 64<<10 || json.Unmarshal(b, out) != nil {
		return errors.New("maintenance host returned an invalid response")
	}
	return nil
}
func (r RPC) Inspect(ctx context.Context, account string) (Snapshot, error) {
	var out Snapshot
	err := r.request(ctx, "maintenance.inspect", account, map[string]any{}, &out)
	return out, err
}
func (r RPC) Warmup(ctx context.Context, account string, windows []string) (ActionResult, error) {
	var out ActionResult
	err := r.request(ctx, "maintenance.warmup", account, map[string]any{"windows": windows}, &out)
	return out, err
}
func (r RPC) Consume(ctx context.Context, account string, p ConsumeRequest) (ActionResult, error) {
	var out ActionResult
	err := r.request(ctx, "maintenance.consume", account, map[string]any{"credit_id": p.CreditID, "redeem_request_id": p.RedeemRequestID, "expected_expires_at": p.ExpectedExpiresAt, "lead_minutes": p.LeadMinutes}, &out)
	return out, err
}
