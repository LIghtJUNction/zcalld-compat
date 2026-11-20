package privacy

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/LIghtJUNction/zcalld-compat/desktop"
)

type firstCount struct {
	Kind      string `json:"kind"`
	NewValues int    `json:"new_values"`
}
type firstEvent struct {
	Version int          `json:"version"`
	EventID string       `json:"event_id"`
	Agent   string       `json:"agent"`
	Counts  []firstCount `json:"counts"`
}

// Never forward arbitrary context labels or custom-rule names to the desktop.
// An unknown classification still contributes to the fixed "OTHER" category.
func firstAgent(agent string) string {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "codex":
		return "codex"
	case "claude", "claude-code":
		return "claude"
	case "opencode":
		return "opencode"
	case "pi":
		return "pi"
	case "zed":
		return "zed"
	case "vscode", "vs-code":
		return "vscode"
	case "cursor":
		return "cursor"
	case "amp":
		return "amp"
	case "continue":
		return "continue"
	case "cline":
		return "cline"
	case "roo", "roo-code":
		return "roo"
	case "kilo", "kilo-code":
		return "kilo"
	case "gemini", "gemini-cli":
		return "gemini"
	case "aider":
		return "aider"
	default:
		return "other"
	}
}
func firstKind(kind string) string {
	switch kind {
	case "AGE_KEY", "API_KEY", "APTOS_KEY", "BANK_CARD", "BIP32_KEY", "BIRTHDAY",
		"BUCKET", "EMAIL", "HOSTNAME", "IBAN", "ID_CARD", "IP", "MAC", "NEAR_KEY",
		"NOSTR_KEY", "PASSPORT", "PASSWORD", "PHONE", "PRIVATE_KEY", "SECRET",
		"SEED_PHRASE", "SERIAL", "SOLANA_KEY", "SOLANA_KEYPAIR", "SSN", "STELLAR_KEY",
		"SUI_KEY", "TOKEN", "USERNAME", "WALLET_KEY", "WIF_KEY", "ZCASH_KEY",
		"NAME", "ADDRESS", "ACCOUNT", "ORGANIZATION", "LOCATION":
		return kind
	default:
		return "OTHER"
	}
}
func makeFirstEvent(agent string, counts map[string]int) (*firstEvent, error) {
	if len(counts) == 0 {
		return nil, nil
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, errFirst
	}
	event := &firstEvent{Version: 1, EventID: hex.EncodeToString(id[:]), Agent: firstAgent(agent)}
	for kind, count := range counts {
		if count > 0 {
			event.Counts = append(event.Counts, firstCount{kind, count})
		}
	}
	if len(event.Counts) == 0 {
		return nil, nil
	}
	sort.Slice(event.Counts, func(a, b int) bool { return event.Counts[a].Kind < event.Counts[b].Kind })
	return event, nil
}
func publishFirst(inbox string, event *firstEvent) error {
	if event == nil {
		return nil
	}
	if desktop.Publish(inbox, 2, event) != nil {
		return errFirst
	}
	return nil
}
