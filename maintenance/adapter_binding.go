package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// DocumentDigest lets an explicit adapter-binding plan bind the exact current
// policy and ledger. It does not expose their private values in the plan.
func DocumentDigest(d Document) string {
	b, _ := json.Marshal(d)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// BindAdapter adds only inactive account policies while preserving all live
// intents, schedules and history. The host rechecks its private identity input
// while the same run lock protects the final compare-and-save operation.
func (s Store) BindAdapter(ctx context.Context, previous, planHash string, adapter json.RawMessage, keys []string, recheck func() error) error {
	if len(previous) != 64 || len(planHash) != 64 || len(adapter) > 1<<20 || !json.Valid(adapter) || len(keys) > MaxAccounts {
		return ErrInvalid
	}
	tx, err := s.acquire(ctx, true)
	if err != nil {
		return err
	}
	defer tx.close()
	if DocumentDigest(tx.doc) != previous {
		return errors.New("adapter binding state changed; inspect a fresh plan")
	}
	if recheck != nil {
		if err = recheck(); err != nil {
			return err
		}
	}
	for _, key := range keys {
		if !accountPattern.MatchString(key) {
			return ErrInvalid
		}
		if _, exists := tx.doc.Policy.Accounts[key]; !exists {
			tx.doc.Policy.Accounts[key] = AccountPolicy{ResetLeadMinutes: 30}
		}
		if tx.doc.Ledger[key] == nil {
			tx.doc.Ledger[key] = &AccountLedger{Windows: map[string]WarmWindow{}, CompletedCards: map[string]time.Time{}}
		}
	}
	tx.doc.Adapter = append(json.RawMessage(nil), adapter...)
	if tx.doc.ImportDigest == "" {
		tx.doc.ImportDigest = planHash
	}
	return tx.save()
}
