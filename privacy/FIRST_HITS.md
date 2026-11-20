# Privacy first-hit events

The privacy worker can publish a local event when it durably records a previously unseen masked or blocked value. This is optional: an absent or empty `desktop_inbox` leaves the existing audit path unchanged, creates no first-hit index and performs no notification-specific file sync. It adds no resident process or polling timer.

The host supplies `desktop_inbox` only when desktop notifications and privacy alerts are explicitly enabled. The consumer owns the private inbox and creates it before use. The publisher does not create a missing inbox.

## Meaning of “first”

Deduplication uses the stable placeholder identity, stored in this index only as a domain-separated SHA-256 digest. Occurrences of the same value in a repeated conversation count once, including when multiple workers process requests concurrently. Engine-local `By == "known"` is not evidence that a value was previously recorded.

This follows the pinned Magpie [first-value semantics](https://github.com/yetone/magpie/blob/1e174f8f9f613a920203a4658d259456fcab5d0d/internal/maskrec/first.go):

- Every recorded placeholder enters the index, including a `warn` observation.
- A warning does not announce masking because its text was left unchanged.
- A fresh masked or blocked value contributes one count to its fixed classification.
- An old warning that later becomes a mask or block is already known.
- A record whose audit values were truncated does not publish a partial count as a complete notification.

The event describes the privacy decision. It does not assert that a later provider request succeeded.

## Payload

The file publisher wraps this payload as a kind-2 event:

```json
{
  "version": 1,
  "event_id": "568dbfdd89bb4af0a19506dc46b8e41aa",
  "agent": "codex",
  "counts": [
    {"kind": "EMAIL", "new_values": 1}
  ]
}
```

The event ID is random. Agent and kind labels are fixed allowlists; unrecognized labels become `other` and `OTHER`. No request text, original value, placeholder, hint, session, path, provider, account or model is sent. A record key is deliberately omitted because the consumer does not need it and nanosecond integers are not exactly representable by every JSON consumer.

## Commit order and recovery

All history/index operations share the existing stable audit lock outside the history directory.

1. Before the new record, validate the index and silently seed any existing history not represented by its file cursors.
2. Append the current audit record, flush the file, and sync the directory on platforms that support directory sync.
3. Write immutable digest buckets, flush them, atomically replace the bounded manifest, and sync the index directory.
4. Release the audit lock, then publish the pure-count event.

The manifest is the index commit point. A process interruption may leave an unreferenced bucket, or an audit record that the manifest has not consumed. Recovery removes only recognized private staging files and silently indexes the already recorded observation. It never replays that observation as a new notification. A committed observation whose notification was lost is not retried.

The index is the private sibling directory `.history.zcalld-first` for a history directory named `history`. It is outside the history payload so a transactional history exchange or rollback does not erase first-seen knowledge. Clearing or pruning audit history also does not clear this index.

History cursors persist a hash of each native file identity (device/inode on Unix; volume/file index on Windows), plus size and modification time. The history directory identity is recorded too. A replacement with preserved timestamps still forces a bounded silent merge; an older manifest without identities is upgraded the same way. Cursors do not assume monotonic record keys: the pinned source can append older keys after newer keys. A new or changed history file is read again and merged silently. On ordinary requests, unchanged history is not rescanned. A new worker verifies committed bucket contents once; within a worker, unchanged immutable-file identities and timestamps allow later requests to avoid rehashing the full index.

The filesystem contract assumes a private directory and cooperating writers using the audit lock. Unsafe nodes, identity changes, incomplete JSONL, malformed state and integrity mismatches suppress notification. File data is flushed before publication; directory-sync behavior follows the existing platform-specific private-file implementation.

## Bounds and failure behavior

| Resource | Bound |
| --- | --- |
| Committed digest buckets | 256 buckets × 64 KiB = 16 MiB |
| Distinct digests in one bucket | 2,048 |
| Transaction staging | At most one new version per bucket, up to another 16 MiB, plus a bounded temporary file |
| Manifest | 64 KiB |
| Historical seed input | 128 MiB across at most 128 daily JSONL files |
| History directory enumeration | At most 384 entries |
| One audit line | 1 MiB |
| One seed merge batch | 4,096 digests |
| First-index directory enumeration | At most 1,024 entries |
| Audit lock wait | Existing 250 ms default |

A bucket can reach its limit before the whole index reaches 16 MiB. The worker does not delete remembered values to make room. Corruption, an exceeded bound, a lock failure or a publisher failure produces a static diagnostic; it does not change the masking result or release a blocked request. Index failure does not disable ordinary audit recording. If the audit itself cannot be persisted, no event is published.

The queue is bounded independently by the desktop publisher. Missing consumers and publication failures can therefore lose a notification. They cannot turn a repeated value into a new first hit or add private content to the event. The consumer owns burst coalescing and notification rate limits.

## Verification

The focused regressions use synthetic values and private temporary directories. They cover repeated requests and restarts, two real worker processes, migrated history including warnings, externally appended older keys, three interrupted commit phases, corrupt/missing/unsafe index nodes, bounded history and buckets, real masking and blocking, unchanged masked output when queue writes fail, and no event after an audit write failure.

No account, provider or model network operation is needed by this producer or its tests.
