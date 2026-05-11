package dynamodb_test

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	fingerprintv1 "github.com/izumin5210/sloff/internal/proto/sloff/fingerprint/v1"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
	ddbpkg "github.com/izumin5210/sloff/internal/sloff/fingerprint/dynamodb"
)

var fixedClock = time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)

// newStorage stands up a DynamoDB-backed Storage pointed at a per-test
// table on the singleton kumo emulator. Production paths route through
// New + the AWS default chain; tests inject the kumo-tailored client via
// WithClient because kumo expects a static `test/test` credential.
func newStorage(t *testing.T, expiresAfterDays int, now time.Time) (*ddbpkg.Storage, string, *awsddb.Client) {
	t.Helper()
	client := newKumoDDBClient()
	table := createTable(t, client)
	st, err := ddbpkg.New(
		context.Background(),
		ddbpkg.Config{
			Table:            table,
			Region:           "us-east-1",
			Endpoint:         kumoEndpoint(),
			ExpiresAfterDays: expiresAfterDays,
		},
		ddbpkg.WithClient(client),
		ddbpkg.WithClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatalf("ddb.New: %v", err)
	}
	return st, table, client
}

func newRecord(taskID string) *fingerprintv1.Record {
	return &fingerprintv1.Record{
		Input:         &fingerprintv1.Input{Hash: "deadbeef"},
		Output:        &fingerprintv1.Output{Hash: "cafebabe"},
		SchemaVersion: fingerprint.SchemaVersion,
		Spec: &fingerprintv1.Spec{
			Cmd:    "echo",
			Dir:    "spec/a",
			TaskId: taskID,
		},
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	st, _, _ := newStorage(t, 0, fixedClock)
	ctx := context.Background()
	key := fingerprint.Key{SpecRelpath: "path/to/spec", TaskID: "gen", InputHash: "deadbeef"}
	rec := newRecord("gen")

	if err := st.Save(ctx, key, rec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := st.Load(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if diff := cmp.Diff(rec, got, protocmp.Transform()); diff != "" {
		t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
	}
}

func TestLoad_MissReturnsFalse(t *testing.T) {
	st, _, _ := newStorage(t, 0, fixedClock)
	ctx := context.Background()
	got, ok, err := st.Load(ctx, fingerprint.Key{SpecRelpath: "x", TaskID: "y", InputHash: "z"})
	if err != nil {
		t.Fatal(err)
	}
	if ok || got != nil {
		t.Errorf("expected miss, got ok=%v rec=%v", ok, got)
	}
}

func TestSave_Overwrite(t *testing.T) {
	st, _, _ := newStorage(t, 0, fixedClock)
	ctx := context.Background()
	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"}

	first := newRecord("gen")
	if err := st.Save(ctx, key, first); err != nil {
		t.Fatal(err)
	}
	second := newRecord("gen")
	second.Output.Hash = "newhash"
	if err := st.Save(ctx, key, second); err != nil {
		t.Fatal(err)
	}

	got, ok, err := st.Load(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if got.GetOutput().GetHash() != "newhash" {
		t.Errorf("expected last-write-wins, got hash=%q", got.GetOutput().GetHash())
	}
}

func TestSave_WritesExpiresAtWhenTTLEnabled(t *testing.T) {
	st, table, client := newStorage(t, 30, fixedClock)
	ctx := context.Background()
	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"}
	if err := st.Save(ctx, key, newRecord("gen")); err != nil {
		t.Fatal(err)
	}
	out, err := client.GetItem(ctx, &awsddb.GetItemInput{
		TableName: aws.String(table),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: key.SpecRelpath},
			"sk": &ddbtypes.AttributeValueMemberS{Value: key.TaskID + "#" + key.InputHash},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	expV, ok := out.Item["expires_at"].(*ddbtypes.AttributeValueMemberN)
	if !ok {
		t.Fatalf("expected expires_at attribute, got %#v", out.Item["expires_at"])
	}
	want := fixedClock.Add(30 * 24 * time.Hour).Unix()
	got, _ := strconv.ParseInt(expV.Value, 10, 64)
	if got != want {
		t.Errorf("expires_at = %d, want %d", got, want)
	}
}

func TestSave_AlwaysWritesCreatedAt(t *testing.T) {
	// created_at must be written regardless of TTL configuration, since
	// ListFilter.OlderThan reads it. Verify both TTL-on and TTL-off
	// modes carry the attribute.
	for _, ttlDays := range []int{0, 30} {
		t.Run(fmt.Sprintf("ttl=%dd", ttlDays), func(t *testing.T) {
			st, table, client := newStorage(t, ttlDays, fixedClock)
			ctx := context.Background()
			key := fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"}
			if err := st.Save(ctx, key, newRecord("gen")); err != nil {
				t.Fatal(err)
			}
			out, err := client.GetItem(ctx, &awsddb.GetItemInput{
				TableName: aws.String(table),
				Key: map[string]ddbtypes.AttributeValue{
					"pk": &ddbtypes.AttributeValueMemberS{Value: key.SpecRelpath},
					"sk": &ddbtypes.AttributeValueMemberS{Value: key.TaskID + "#" + key.InputHash},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			cAttr, ok := out.Item["created_at"].(*ddbtypes.AttributeValueMemberN)
			if !ok {
				t.Fatalf("expected created_at attribute, got %#v", out.Item["created_at"])
			}
			want := fixedClock.Unix()
			got, _ := strconv.ParseInt(cAttr.Value, 10, 64)
			if got != want {
				t.Errorf("created_at = %d, want %d", got, want)
			}
		})
	}
}

func TestSave_NoTTLAttributeWhenDisabled(t *testing.T) {
	st, table, client := newStorage(t, 0, fixedClock)
	ctx := context.Background()
	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"}
	if err := st.Save(ctx, key, newRecord("gen")); err != nil {
		t.Fatal(err)
	}
	out, err := client.GetItem(ctx, &awsddb.GetItemInput{
		TableName: aws.String(table),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: key.SpecRelpath},
			"sk": &ddbtypes.AttributeValueMemberS{Value: key.TaskID + "#" + key.InputHash},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := out.Item["expires_at"]; exists {
		t.Errorf("expected expires_at to be absent when TTL disabled")
	}
}

func TestDelete_RemovesItem(t *testing.T) {
	st, _, _ := newStorage(t, 0, fixedClock)
	ctx := context.Background()
	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"}
	if err := st.Save(ctx, key, newRecord("gen")); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := st.Load(ctx, key); ok {
		t.Error("expected miss after Delete")
	}
}

func TestDelete_MissingKeyIsNoop(t *testing.T) {
	st, _, _ := newStorage(t, 0, fixedClock)
	if err := st.Delete(context.Background(), fingerprint.Key{SpecRelpath: "s", TaskID: "t", InputHash: "h"}); err != nil {
		t.Errorf("Delete on missing key should be noop, got %v", err)
	}
}

func TestList_AllAndFiltered(t *testing.T) {
	st, _, _ := newStorage(t, 0, fixedClock)
	ctx := context.Background()
	keys := []fingerprint.Key{
		{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h1"},
		{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h2"},
		{SpecRelpath: "spec/a", TaskID: "other", InputHash: "h3"},
		{SpecRelpath: "spec/b", TaskID: "gen", InputHash: "h4"},
	}
	for _, k := range keys {
		if err := st.Save(ctx, k, newRecord(k.TaskID)); err != nil {
			t.Fatal(err)
		}
	}

	all, err := st.List(ctx, fingerprint.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(keys) {
		t.Errorf("expected %d keys total, got %d: %+v", len(keys), len(all), all)
	}

	bySpec, err := st.List(ctx, fingerprint.ListFilter{SpecRelpath: "spec/a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bySpec) != 3 {
		t.Errorf("expected 3 keys for spec/a, got %d: %+v", len(bySpec), bySpec)
	}

	byTask, err := st.List(ctx, fingerprint.ListFilter{SpecRelpath: "spec/a", TaskID: "gen"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byTask) != 2 {
		t.Errorf("expected 2 gen variants under spec/a, got %d: %+v", len(byTask), byTask)
	}
}

// TestList_OlderThanComparesAgainstCreatedAt locks the contract that
// ListFilter.OlderThan is a write-time cutoff, not a TTL cutoff. With
// TTL enabled the two timestamps differ by `ExpiresAfterDays`, so a
// bug that compares against `expires_at` would pass cutoffs near
// `fixedClock` but fail cutoffs near `fixedClock + 30d`.
func TestList_OlderThanComparesAgainstCreatedAt(t *testing.T) {
	st, _, _ := newStorage(t, 30, fixedClock)
	ctx := context.Background()
	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"}
	if err := st.Save(ctx, key, newRecord("gen")); err != nil {
		t.Fatal(err)
	}

	got, err := st.List(ctx, fingerprint.ListFilter{OlderThan: fixedClock.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("expected key included when created_at < cutoff, got %+v", got)
	}

	got, err = st.List(ctx, fingerprint.ListFilter{OlderThan: fixedClock.Add(-time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected key excluded when created_at >= cutoff, got %+v", got)
	}

	// Sanity check: a cutoff that is *after* expires_at must still
	// exclude the recent entry (its created_at is `fixedClock`, well
	// before that cutoff would be considered "old enough" — but the
	// previous bug compared against expires_at and would have wrongly
	// included it).
	expiresAt := fixedClock.Add(30 * 24 * time.Hour)
	got, err = st.List(ctx, fingerprint.ListFilter{OlderThan: expiresAt.Add(-time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("expected key included when created_at < cutoff < expires_at, got %+v", got)
	}
}

func TestCollapseDuplicates_AlwaysZero(t *testing.T) {
	st, _, _ := newStorage(t, 0, fixedClock)
	removed, err := st.CollapseDuplicates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("expected zero collapses (DynamoDB has no duplicates), got %d", removed)
	}
}

func TestLoadMany(t *testing.T) {
	st, _, _ := newStorage(t, 0, fixedClock)
	ctx := context.Background()

	keys := []fingerprint.Key{
		{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h1"},
		{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h2"},
		{SpecRelpath: "spec/b", TaskID: "other", InputHash: "h3"},
	}
	for _, k := range keys {
		if err := st.Save(ctx, k, newRecord(k.TaskID)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.LoadMany(ctx, append(keys, fingerprint.Key{SpecRelpath: "missing", TaskID: "x", InputHash: "y"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(keys) {
		t.Errorf("expected %d records (missing key excluded), got %d", len(keys), len(got))
	}
	for _, k := range keys {
		if _, ok := got[k]; !ok {
			t.Errorf("missing %+v in LoadMany result", k)
		}
	}
}

func TestLoadMany_ExceedsBatchSize(t *testing.T) {
	st, _, _ := newStorage(t, 0, fixedClock)
	ctx := context.Background()
	// 150 keys => 2 BatchGetItem calls (limit 100).
	const N = 150
	var keys []fingerprint.Key
	for i := range N {
		keys = append(keys, fingerprint.Key{
			SpecRelpath: "spec",
			TaskID:      "gen",
			InputHash:   "h" + strconv.Itoa(i),
		})
	}
	items := make([]fingerprint.KeyRecord, 0, N)
	for _, k := range keys {
		items = append(items, fingerprint.KeyRecord{Key: k, Record: newRecord(k.TaskID)})
	}
	if err := st.SaveMany(ctx, items); err != nil {
		t.Fatal(err)
	}

	got, err := st.LoadMany(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != N {
		t.Errorf("expected %d records, got %d", N, len(got))
	}
}

func TestSaveMany(t *testing.T) {
	st, _, _ := newStorage(t, 0, fixedClock)
	ctx := context.Background()
	items := []fingerprint.KeyRecord{
		{Key: fingerprint.Key{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h1"}, Record: newRecord("gen")},
		{Key: fingerprint.Key{SpecRelpath: "spec/b", TaskID: "other", InputHash: "h2"}, Record: newRecord("other")},
	}
	if err := st.SaveMany(ctx, items); err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if _, ok, _ := st.Load(ctx, it.Key); !ok {
			t.Errorf("expected hit for %+v after SaveMany", it.Key)
		}
	}
}

func TestSaveMany_ExceedsBatchSize(t *testing.T) {
	st, _, _ := newStorage(t, 0, fixedClock)
	ctx := context.Background()
	// 60 items => 3 BatchWriteItem calls (limit 25).
	const N = 60
	items := make([]fingerprint.KeyRecord, 0, N)
	for i := range N {
		items = append(items, fingerprint.KeyRecord{
			Key:    fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h" + strconv.Itoa(i)},
			Record: newRecord("gen"),
		})
	}
	if err := st.SaveMany(ctx, items); err != nil {
		t.Fatal(err)
	}
	all, err := st.List(ctx, fingerprint.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != N {
		sort.Slice(all, func(i, j int) bool { return all[i].InputHash < all[j].InputHash })
		t.Errorf("expected %d items in table, got %d (first: %+v)", N, len(all), all[:min(3, len(all))])
	}
}

func TestSaveMany_EmptyIsNoop(t *testing.T) {
	st, _, _ := newStorage(t, 0, fixedClock)
	if err := st.SaveMany(context.Background(), nil); err != nil {
		t.Errorf("SaveMany on empty should be noop, got %v", err)
	}
}

func TestLoadMany_EmptyKeys(t *testing.T) {
	st, _, _ := newStorage(t, 0, fixedClock)
	got, err := st.LoadMany(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMany on empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %d entries", len(got))
	}
}

func TestLoad_NonExistentTable(t *testing.T) {
	client := newKumoDDBClient()
	st, err := ddbpkg.New(
		context.Background(),
		ddbpkg.Config{Table: "no-such-table", Region: "us-east-1", Endpoint: kumoEndpoint()},
		ddbpkg.WithClient(client),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = st.Load(context.Background(), fingerprint.Key{SpecRelpath: "x", TaskID: "y", InputHash: "z"})
	if err == nil {
		t.Error("expected error for non-existent table")
	}
}

func TestNew_RequiresTable(t *testing.T) {
	if _, err := ddbpkg.New(context.Background(), ddbpkg.Config{}); err == nil {
		t.Error("expected error when table is empty")
	}
}

func TestNew_RejectsNegativeTTL(t *testing.T) {
	if _, err := ddbpkg.New(context.Background(), ddbpkg.Config{Table: "t", ExpiresAfterDays: -1}); err == nil {
		t.Error("expected error for negative ExpiresAfterDays")
	}
}

// TestNew_GoesThroughDefaultAWSChain exercises the production code path
// where New constructs an aws.Config via LoadDefaultConfig itself
// (instead of receiving a pre-built client through WithClient).
// Credential resolution is lazy in aws-sdk-go-v2 so this does not need
// real AWS credentials to succeed at construction time.
func TestNew_GoesThroughDefaultAWSChain(t *testing.T) {
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "stub")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "stub")
	st, err := ddbpkg.New(context.Background(), ddbpkg.Config{
		Table:    "stub",
		Region:   "us-east-1",
		Endpoint: kumoEndpoint(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if st.Name() != "dynamodb" {
		t.Errorf("Name() = %q, want dynamodb", st.Name())
	}
}

func TestName(t *testing.T) {
	st, _, _ := newStorage(t, 0, fixedClock)
	if name := st.Name(); name != "dynamodb" {
		t.Errorf("Name() = %q, want dynamodb", name)
	}
}
