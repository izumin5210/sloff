// Package dynamodb is a fingerprint.Storage backend that persists records as
// individual items in an Amazon DynamoDB table. Used when a repo's
// .sloff/config.yml selects `fingerprint.backend: dynamodb`. The wire format
// is identical to the local backend (proto binary, ADR-0009); only the
// physical layout differs (per-item KV instead of per-file ADR-0010 paths).
//
// Schema (table the user pre-creates):
//
//	pk (S)         partition key, holds spec_relpath
//	sk (S)         sort key, holds "<task_id>#<input_hash>"
//	record (B)     deterministic protobuf bytes
//	expires_at (N) optional, Unix epoch seconds for DynamoDB TTL
package dynamodb

import (
	"strings"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
)

// pkAttr / skAttr are the DynamoDB attribute names the table schema uses.
// Kept as constants because the runtime never queries on different keys.
const (
	pkAttr        = "pk"
	skAttr        = "sk"
	recordAttr    = "record"
	expiresAtAttr = "expires_at"

	// skSeparator splits task_id from input_hash inside the composite sort
	// key. ADR-0008's slug grammar guarantees task_id has no '#', so the
	// split is unambiguous.
	skSeparator = "#"
)

func partitionKey(key fingerprint.Key) string { return key.SpecRelpath }

func sortKey(key fingerprint.Key) string {
	return key.TaskID + skSeparator + key.InputHash
}

// parseSortKey splits a stored sort-key string back into (task_id,
// input_hash). Returns ok=false for malformed keys so List/Scan can skip
// stray items rather than synthesise bogus Keys.
func parseSortKey(s string) (taskID, inputHash string, ok bool) {
	idx := strings.Index(s, skSeparator)
	if idx <= 0 || idx == len(s)-1 {
		return "", "", false
	}
	return s[:idx], s[idx+1:], true
}

// sortKeyTaskPrefix is the begins_with argument used by Query when the
// caller filters on TaskID. Trailing separator ensures "y#" only matches
// "y#<hash>", not "yy#<hash>" or "y" by itself.
func sortKeyTaskPrefix(taskID string) string {
	return taskID + skSeparator
}
