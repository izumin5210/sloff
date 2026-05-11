package dynamodb

import (
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
)

func TestDecodeItem_RejectsMissingRecordAttribute(t *testing.T) {
	if _, err := decodeItem(map[string]ddbtypes.AttributeValue{}); err == nil {
		t.Error("expected error when record attribute is missing")
	}
}

func TestDecodeItem_RejectsWrongType(t *testing.T) {
	item := map[string]ddbtypes.AttributeValue{
		recordAttr: &ddbtypes.AttributeValueMemberS{Value: "not bytes"},
	}
	if _, err := decodeItem(item); err == nil {
		t.Error("expected error when record attribute is not Binary")
	}
}

func TestPkSkFromItem_RejectsMissingAttrs(t *testing.T) {
	cases := []map[string]ddbtypes.AttributeValue{
		{},
		{pkAttr: &ddbtypes.AttributeValueMemberS{Value: "spec"}}, // sk missing
		{skAttr: &ddbtypes.AttributeValueMemberS{Value: "task#h"}},
		{
			pkAttr: &ddbtypes.AttributeValueMemberN{Value: "1"}, // wrong type
			skAttr: &ddbtypes.AttributeValueMemberS{Value: "task#h"},
		},
	}
	for i, item := range cases {
		if _, _, ok := pkSkFromItem(item); ok {
			t.Errorf("case %d: expected ok=false, item=%#v", i, item)
		}
	}
}

func TestKeyFromItem_RejectsMalformedSortKey(t *testing.T) {
	item := map[string]ddbtypes.AttributeValue{
		pkAttr: &ddbtypes.AttributeValueMemberS{Value: "spec"},
		skAttr: &ddbtypes.AttributeValueMemberS{Value: "no-separator"},
	}
	_, ok, _ := keyFromItem(item, &fingerprint.ListFilter{})
	if ok {
		t.Error("expected keyFromItem to skip items with malformed sort key")
	}
}

func TestKeyFromItem_KeepsItemWithoutCreatedAtUnderOlderThan(t *testing.T) {
	item := map[string]ddbtypes.AttributeValue{
		pkAttr: &ddbtypes.AttributeValueMemberS{Value: "spec"},
		skAttr: &ddbtypes.AttributeValueMemberS{Value: "task#h"},
	}
	_, ok, err := keyFromItem(item, &fingerprint.ListFilter{OlderThan: time.Now()})
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

func TestKeyFromItem_PropagatesReadCreatedAtError(t *testing.T) {
	item := map[string]ddbtypes.AttributeValue{
		pkAttr:        &ddbtypes.AttributeValueMemberS{Value: "spec"},
		skAttr:        &ddbtypes.AttributeValueMemberS{Value: "task#h"},
		createdAtAttr: &ddbtypes.AttributeValueMemberS{Value: "not-a-number"},
	}
	if _, _, err := keyFromItem(item, &fingerprint.ListFilter{OlderThan: time.Now()}); err == nil {
		t.Error("expected error when created_at is the wrong type")
	}
}

func TestReadUnixNumber_AbsentReturnsFalse(t *testing.T) {
	got, ok, err := readUnixNumber(map[string]ddbtypes.AttributeValue{}, createdAtAttr)
	if err != nil {
		t.Fatal(err)
	}
	if ok || !got.IsZero() {
		t.Errorf("expected (zero, false, nil), got (%v, %v)", got, ok)
	}
}

func TestReadUnixNumber_RejectsNonNumeric(t *testing.T) {
	item := map[string]ddbtypes.AttributeValue{
		createdAtAttr: &ddbtypes.AttributeValueMemberN{Value: "abc"},
	}
	if _, _, err := readUnixNumber(item, createdAtAttr); err == nil {
		t.Error("expected error for non-numeric value")
	}
}

func TestReadUnixNumber_RejectsWrongType(t *testing.T) {
	item := map[string]ddbtypes.AttributeValue{
		createdAtAttr: &ddbtypes.AttributeValueMemberS{Value: "1234"},
	}
	if _, _, err := readUnixNumber(item, createdAtAttr); err == nil {
		t.Error("expected error when attribute is not Number type")
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
