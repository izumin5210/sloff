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
//	created_at (N) Unix epoch seconds at write time; drives ListFilter.OlderThan
//	expires_at (N) optional, Unix epoch seconds for DynamoDB TTL
//
// created_at is written unconditionally so List(OlderThan: ...) has a
// stable timestamp to filter on regardless of whether TTL is enabled.
// expires_at is written only when ExpiresAfterDays > 0; the TTL setting
// on the table consumes it for auto-eviction independent of List.
package dynamodb

import (
	"strings"
	"time"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
)

// Attribute names the DynamoDB schema uses. Kept as constants because the
// runtime never queries on different names and several of them appear in
// the typed-struct tags below.
const (
	pkAttr        = "pk"
	skAttr        = "sk"
	recordAttr    = "record"
	createdAtAttr = "created_at"
	expiresAtAttr = "expires_at"

	// skSeparator splits task_id from input_hash inside the composite sort
	// key. ADR-0008's slug grammar guarantees task_id has no '#', so the
	// split is unambiguous.
	skSeparator = "#"
)

// item is the typed projection of a DynamoDB item stored by the backend.
// Marshalling goes through aws-sdk-go-v2/feature/dynamodb/attributevalue
// so the schema lives on the struct tags instead of being open-coded
// across encode / decode / scan paths.
//
// CreatedAt is always populated (it backs ListFilter.OlderThan); ExpiresAt
// is a pointer so the omitempty tag can drop it from the marshalled map
// when TTL is disabled. unixtime tells attributevalue to serialize each
// time.Time as a Number attribute containing Unix epoch seconds.
type item struct {
	PK        string     `dynamodbav:"pk"`
	SK        string     `dynamodbav:"sk"`
	Record    []byte     `dynamodbav:"record"`
	CreatedAt time.Time  `dynamodbav:"created_at,unixtime"`
	ExpiresAt *time.Time `dynamodbav:"expires_at,unixtime,omitempty"`
}

// primaryKey is the {pk, sk} subset of item, used as the request payload
// for GetItem / DeleteItem / BatchGetItem where the body attributes are
// irrelevant.
type primaryKey struct {
	PK string `dynamodbav:"pk"`
	SK string `dynamodbav:"sk"`
}

func newPrimaryKey(k fingerprint.Key) primaryKey {
	return primaryKey{PK: k.SpecRelpath, SK: sortKey(k)}
}

// sortKey is the on-disk encoding of the (TaskID, InputHash) tuple as a
// single sort-key string.
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

// toKey reconstructs the fingerprint.Key from the decoded item. Returns
// ok=false for malformed sort keys so callers can skip foreign items
// without producing bogus Keys.
func (i item) toKey() (fingerprint.Key, bool) {
	taskID, hash, ok := parseSortKey(i.SK)
	if !ok {
		return fingerprint.Key{}, false
	}
	return fingerprint.Key{SpecRelpath: i.PK, TaskID: taskID, InputHash: hash}, true
}
