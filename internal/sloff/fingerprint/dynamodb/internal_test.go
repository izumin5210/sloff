package dynamodb

import (
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
)

func TestRecordFromAttrs_RejectsMissingRecord(t *testing.T) {
	// pk / sk present but record bytes absent: itemFromAttrs succeeds
	// (record decodes to a nil []byte), recordFromAttrs surfaces the
	// missing-attribute error so callers don't synthesise an empty proto.
	attrs := map[string]ddbtypes.AttributeValue{
		pkAttr: &ddbtypes.AttributeValueMemberS{Value: "spec"},
		skAttr: &ddbtypes.AttributeValueMemberS{Value: "task#h"},
	}
	if _, err := recordFromAttrs(attrs); err == nil {
		t.Error("expected error when record attribute is missing")
	}
}

func TestRecordFromAttrs_RejectsCorruptProtoBytes(t *testing.T) {
	attrs := map[string]ddbtypes.AttributeValue{
		pkAttr:     &ddbtypes.AttributeValueMemberS{Value: "spec"},
		skAttr:     &ddbtypes.AttributeValueMemberS{Value: "task#h"},
		recordAttr: &ddbtypes.AttributeValueMemberB{Value: []byte("not a proto")},
	}
	if _, err := recordFromAttrs(attrs); err == nil {
		t.Error("expected proto decode error for corrupt record bytes")
	}
}

func TestKeyFromAttrs_SkipsMalformedSortKey(t *testing.T) {
	attrs := map[string]ddbtypes.AttributeValue{
		pkAttr: &ddbtypes.AttributeValueMemberS{Value: "spec"},
		skAttr: &ddbtypes.AttributeValueMemberS{Value: "no-separator"},
	}
	_, ok, err := keyFromAttrs(attrs, &fingerprint.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected keyFromAttrs to skip items with malformed sort key")
	}
}

func TestKeyFromAttrs_KeepsItemWithoutCreatedAtUnderOlderThan(t *testing.T) {
	attrs := map[string]ddbtypes.AttributeValue{
		pkAttr: &ddbtypes.AttributeValueMemberS{Value: "spec"},
		skAttr: &ddbtypes.AttributeValueMemberS{Value: "task#h"},
	}
	_, ok, err := keyFromAttrs(attrs, &fingerprint.ListFilter{OlderThan: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	// "Conservative keep" branch: items predating the created_at
	// attribute (legacy entries) should not silently disappear from
	// list-based GC sweeps.
	if !ok {
		t.Error("expected item without created_at to be kept under OlderThan filter")
	}
}

func TestKeyFromAttrs_PropagatesUnmarshalError(t *testing.T) {
	// CreatedAt is declared as time.Time with `,unixtime`, so a String
	// value at that attribute fails the SDK's typed unmarshal.
	attrs := map[string]ddbtypes.AttributeValue{
		pkAttr:        &ddbtypes.AttributeValueMemberS{Value: "spec"},
		skAttr:        &ddbtypes.AttributeValueMemberS{Value: "task#h"},
		createdAtAttr: &ddbtypes.AttributeValueMemberS{Value: "not-a-number"},
	}
	if _, _, err := keyFromAttrs(attrs, &fingerprint.ListFilter{OlderThan: time.Now()}); err == nil {
		t.Error("expected error when created_at is the wrong type")
	}
}

func TestUnprocessedKeys_FiltersUnknownAndMalformed(t *testing.T) {
	known := fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"}
	byPair := map[[2]string]fingerprint.Key{{known.SpecRelpath, sortKey(known)}: known}

	left := ddbtypes.KeysAndAttributes{
		Keys: []map[string]ddbtypes.AttributeValue{
			// known
			{
				pkAttr: &ddbtypes.AttributeValueMemberS{Value: "spec"},
				skAttr: &ddbtypes.AttributeValueMemberS{Value: "gen#h"},
			},
			// unknown pk/sk pair (not in original LoadMany request)
			{
				pkAttr: &ddbtypes.AttributeValueMemberS{Value: "other"},
				skAttr: &ddbtypes.AttributeValueMemberS{Value: "task#h2"},
			},
			// malformed (pk missing)
			{
				skAttr: &ddbtypes.AttributeValueMemberS{Value: "gen#h"},
			},
		},
	}
	got := unprocessedKeys(left, byPair)
	if len(got) != 1 || got[0] != known {
		t.Errorf("unprocessedKeys = %+v, want exactly the known key", got)
	}
}

func TestChunkKeys_ZeroSizeReturnsSingleChunk(t *testing.T) {
	in := []fingerprint.Key{{SpecRelpath: "s"}}
	got := chunkKeys(in, 0)
	if len(got) != 1 || len(got[0]) != 1 {
		t.Errorf("chunkKeys with size=0 should return one chunk holding everything, got %+v", got)
	}
}

func TestChunkItems_ZeroSizeReturnsSingleChunk(t *testing.T) {
	in := []fingerprint.KeyRecord{{Key: fingerprint.Key{SpecRelpath: "s"}}}
	got := chunkItems(in, 0)
	if len(got) != 1 || len(got[0]) != 1 {
		t.Errorf("chunkItems with size=0 should return one chunk holding everything, got %+v", got)
	}
}
