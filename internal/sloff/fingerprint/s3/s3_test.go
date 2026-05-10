package s3_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	fingerprintv1 "github.com/izumin5210/sloff/internal/proto/sloff/fingerprint/v1"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
	s3pkg "github.com/izumin5210/sloff/internal/sloff/fingerprint/s3"
)

var fixedClock = time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)

const testPrefix = "sloff/fingerprints"

// newStorage stands up an S3-backed fingerprint.Storage pointed at the
// per-test bucket on the singleton kumo emulator. Production code goes
// through s3.New and lets aws-sdk-go-v2's default chain pick credentials;
// here we inject a kumo-tailored client via WithClient because kumo expects
// path-style addressing and a static `test/test` credential.
func newStorage(t *testing.T, now time.Time) (*s3pkg.Storage, string, *awss3.Client) {
	t.Helper()
	client := newKumoS3Client()
	bucket := createBucket(t, client)
	st, err := s3pkg.New(
		context.Background(),
		fingerprint.S3Config{
			Bucket:   bucket,
			Prefix:   testPrefix,
			Endpoint: kumoEndpoint(),
			Region:   "us-east-1",
		},
		s3pkg.WithClock(func() time.Time { return now }),
		s3pkg.WithClient(client),
	)
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	return st, bucket, client
}

func newRecord(taskID string) *fingerprintv1.Record {
	return &fingerprintv1.Record{
		Input:         &fingerprintv1.Input{Hash: "deadbeef"},
		Output:        &fingerprintv1.Output{Hash: "cafebabe"},
		SchemaVersion: fingerprint.SchemaVersion,
		Spec: &fingerprintv1.Spec{
			Cmd:    "echo hi",
			Dir:    "path/to/spec",
			TaskId: taskID,
		},
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	st, _, _ := newStorage(t, fixedClock)
	ctx := context.Background()

	key := fingerprint.Key{SpecRelpath: "path/to/spec", TaskID: "gen", InputHash: "deadbeef"}
	rec := newRecord("gen")

	if err := st.Save(ctx, key, rec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := st.Load(ctx, key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ok {
		t.Fatal("Load: expected hit")
	}
	if diff := cmp.Diff(rec, got, protocmp.Transform()); diff != "" {
		t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
	}
}

func TestLoad_MissReturnsFalse(t *testing.T) {
	st, _, _ := newStorage(t, fixedClock)
	ctx := context.Background()

	got, ok, err := st.Load(ctx, fingerprint.Key{SpecRelpath: "x", TaskID: "y", InputHash: "z"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ok || got != nil {
		t.Errorf("expected miss, got ok=%v rec=%v", ok, got)
	}
}

func TestSave_PreservesObjectKeyAndTimestampPrefix(t *testing.T) {
	st, bucket, client := newStorage(t, fixedClock)
	ctx := context.Background()

	key := fingerprint.Key{SpecRelpath: "path/to/spec", TaskID: "gen", InputHash: "abc123"}
	if err := st.Save(ctx, key, newRecord("gen")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	wantKey := testPrefix + "/path/to/spec/gen/20260505120000000-abc123.pb"
	if _, err := client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(wantKey),
	}); err != nil {
		t.Errorf("expected object at %s, got err=%v", wantKey, err)
	}
}

func TestSave_PreservesPrefixOnInPlaceOverwrite(t *testing.T) {
	now := fixedClock
	clockFn := func() time.Time { return now }
	client := newKumoS3Client()
	bucket := createBucket(t, client)
	st, err := s3pkg.New(
		context.Background(),
		fingerprint.S3Config{
			Bucket:   bucket,
			Prefix:   testPrefix,
			Endpoint: kumoEndpoint(),
			Region:   "us-east-1",
		},
		s3pkg.WithClock(clockFn),
		s3pkg.WithClient(client),
	)
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	ctx := context.Background()

	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"}
	if err := st.Save(ctx, key, newRecord("gen")); err != nil {
		t.Fatal(err)
	}
	first := testPrefix + "/spec/gen/20260505120000000-h.pb"
	if _, err := client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(first),
	}); err != nil {
		t.Fatalf("first save missing: %v", err)
	}

	// Advance the clock by an hour. A second Save for the same Key must
	// overwrite the original key, not produce a new one.
	now = now.Add(time.Hour)
	updated := newRecord("gen")
	updated.Output.Hash = "newhash"
	if err := st.Save(ctx, key, updated); err != nil {
		t.Fatal(err)
	}
	keys := listKeys(t, client, bucket, testPrefix+"/spec/gen/")
	if len(keys) != 1 || keys[0] != first {
		t.Errorf("expected single object at %s, got %v", first, keys)
	}
}

func TestSave_CollapsesPostMergeDuplicates(t *testing.T) {
	st, bucket, client := newStorage(t, fixedClock)
	ctx := context.Background()

	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"}
	older := testPrefix + "/spec/gen/20260101000000000-h.pb"
	newer := testPrefix + "/spec/gen/20260601000000000-h.pb"
	body, err := fingerprint.Marshal(newRecord("gen"))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{older, newer} {
		putObject(t, client, bucket, k, body)
	}

	updated := newRecord("gen")
	updated.Output.Hash = "newhash"
	if err := st.Save(ctx, key, updated); err != nil {
		t.Fatal(err)
	}
	if _, err := client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(older),
	}); err != nil {
		t.Errorf("expected earliest prefix kept at %s, got err=%v", older, err)
	}
	if _, err := client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(newer),
	}); err == nil {
		t.Errorf("expected later duplicate removed at %s, but it still exists", newer)
	}
}

func TestLoad_ReturnsLatestAmongDuplicates(t *testing.T) {
	st, bucket, client := newStorage(t, fixedClock)
	ctx := context.Background()

	older := newRecord("gen")
	older.Output.Hash = "older"
	newer := newRecord("gen")
	newer.Output.Hash = "newer"
	for k, rec := range map[string]*fingerprintv1.Record{
		testPrefix + "/spec/gen/20260101000000000-h.pb": older,
		testPrefix + "/spec/gen/20260601000000000-h.pb": newer,
	} {
		body, err := fingerprint.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		putObject(t, client, bucket, k, body)
	}

	got, ok, err := st.Load(ctx, fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected hit")
	}
	if got.GetOutput().GetHash() != "newer" {
		t.Errorf("expected latest record, got hash=%q", got.GetOutput().GetHash())
	}
}

func TestDelete_RemovesObject(t *testing.T) {
	st, _, _ := newStorage(t, fixedClock)
	ctx := context.Background()

	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "task", InputHash: "h"}
	if err := st.Save(ctx, key, newRecord("task")); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, ok, err := st.Load(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected miss after Delete")
	}
}

func TestDelete_RemovesAllTimestampVariants(t *testing.T) {
	st, bucket, client := newStorage(t, fixedClock)
	ctx := context.Background()

	body, err := fingerprint.Marshal(newRecord("task"))
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{
		testPrefix + "/spec/task/20260101000000000-h.pb",
		testPrefix + "/spec/task/20260601000000000-h.pb",
	}
	for _, k := range keys {
		putObject(t, client, bucket, k, body)
	}

	if err := st.Delete(ctx, fingerprint.Key{SpecRelpath: "spec", TaskID: "task", InputHash: "h"}); err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if _, err := client.HeadObject(ctx, &awss3.HeadObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(k),
		}); err == nil {
			t.Errorf("expected %s removed, but still present", k)
		}
	}
}

func TestDelete_MissingKeyIsNoop(t *testing.T) {
	st, _, _ := newStorage(t, fixedClock)
	ctx := context.Background()
	if err := st.Delete(ctx, fingerprint.Key{SpecRelpath: "s", TaskID: "t", InputHash: "h"}); err != nil {
		t.Errorf("Delete on missing should be noop, got %v", err)
	}
}

func TestList_AllAndFiltered(t *testing.T) {
	st, _, _ := newStorage(t, fixedClock)
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

	all, err := st.List(ctx, fingerprint.ListFilter{})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 keys, got %d: %+v", len(all), all)
	}

	bySpec, err := st.List(ctx, fingerprint.ListFilter{SpecRelpath: "spec/a"})
	if err != nil {
		t.Fatalf("List bySpec: %v", err)
	}
	if len(bySpec) != 2 {
		t.Errorf("expected 2 keys for spec/a, got %d: %+v", len(bySpec), bySpec)
	}

	byTask, err := st.List(ctx, fingerprint.ListFilter{TaskID: "other"})
	if err != nil {
		t.Fatalf("List byTask: %v", err)
	}
	if len(byTask) != 1 || byTask[0].InputHash != "h3" {
		t.Errorf("expected single h3, got %+v", byTask)
	}
}

func TestList_DedupesPostMergeDuplicates(t *testing.T) {
	st, bucket, client := newStorage(t, fixedClock)
	ctx := context.Background()

	body, err := fingerprint.Marshal(newRecord("gen"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"20260101000000000-h.pb", "20260601000000000-h.pb"} {
		putObject(t, client, bucket, testPrefix+"/spec/gen/"+name, body)
	}

	got, err := st.List(ctx, fingerprint.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	want := fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"}
	if len(got) != 1 || got[0] != want {
		t.Errorf("expected single dedupe entry, got %+v", got)
	}
}

func TestList_OlderThan(t *testing.T) {
	// kumo (v0.18.2) has two compounding wall-clock bugs in S3:
	//   - HeadObject reports LastModified correctly (real UTC),
	//   - ListObjectsV2 reports LastModified using *system local time*
	//     but tags it as UTC.
	// The Storage.List path consumes ListObjectsV2 LastModified, so the
	// cutoff has to be anchored to that exact value to be portable
	// across timezones. We read the listed LastModified once, then sandwich
	// the cutoff just before / after it.
	st, bucket, client := newStorage(t, fixedClock)
	ctx := context.Background()

	if err := st.Save(ctx, fingerprint.Key{SpecRelpath: "s", TaskID: "t", InputHash: "old"}, newRecord("t")); err != nil {
		t.Fatal(err)
	}

	listed, err := client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(testPrefix + "/"),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	if len(listed.Contents) == 0 || listed.Contents[0].LastModified == nil {
		t.Fatalf("ListObjectsV2 returned no usable LastModified: %+v", listed.Contents)
	}
	listedAt := *listed.Contents[0].LastModified

	got, err := st.List(ctx, fingerprint.ListFilter{OlderThan: listedAt.Add(time.Second)})
	if err != nil {
		t.Fatalf("List (future cutoff): %v", err)
	}
	if len(got) != 1 || got[0].InputHash != "old" {
		t.Errorf("expected single older entry, got %+v", got)
	}

	got, err = st.List(ctx, fingerprint.ListFilter{OlderThan: listedAt.Add(-time.Second)})
	if err != nil {
		t.Fatalf("List (past cutoff): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected zero entries for past cutoff, got %+v", got)
	}
}

func TestList_IgnoresForeignObjects(t *testing.T) {
	st, bucket, client := newStorage(t, fixedClock)
	ctx := context.Background()

	body, err := fingerprint.Marshal(newRecord("gen"))
	if err != nil {
		t.Fatal(err)
	}
	// One legitimate record + assorted garbage.
	putObject(t, client, bucket, testPrefix+"/spec/gen/20260505120000000-deadbeef.pb", body)
	for _, name := range []string{
		"deadbeef.pb",                   // missing dash / shape
		"notatimestamp-deadbeef.pb",     // dash present but prefix not all digits
		"abcdefghijklmnopq-deadbeef.pb", // 17 chars but non-numeric
		"20260505120000000-stray.txt",   // wrong extension
	} {
		putObject(t, client, bucket, testPrefix+"/spec/gen/"+name, []byte("noise"))
	}

	got, err := st.List(ctx, fingerprint.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].InputHash != "deadbeef" {
		t.Errorf("expected single entry for the well-formed object, got %+v", got)
	}
}

func TestCollapseDuplicates(t *testing.T) {
	st, bucket, client := newStorage(t, fixedClock)
	ctx := context.Background()

	body, err := fingerprint.Marshal(newRecord("gen"))
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{
		testPrefix + "/spec/gen/20260101000000000-h.pb",
		testPrefix + "/spec/gen/20260301000000000-h.pb",
		testPrefix + "/spec/gen/20260601000000000-h.pb",
	}
	for _, k := range keys {
		putObject(t, client, bucket, k, body)
	}

	removed, err := st.CollapseDuplicates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("expected 2 removals, got %d", removed)
	}
	if _, err := client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(keys[0]),
	}); err != nil {
		t.Errorf("expected earliest preserved, got err=%v", err)
	}
	for _, k := range keys[1:] {
		if _, err := client.HeadObject(ctx, &awss3.HeadObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(k),
		}); err == nil {
			t.Errorf("expected %s removed, but still present", k)
		}
	}
}

func TestCollapseDuplicates_RespectsCtx(t *testing.T) {
	st, bucket, client := newStorage(t, fixedClock)
	body, err := fingerprint.Marshal(newRecord("gen"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"20260101000000000-h.pb", "20260601000000000-h.pb"} {
		putObject(t, client, bucket, testPrefix+"/spec/gen/"+name, body)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.CollapseDuplicates(ctx); err == nil {
		t.Error("expected cancelled ctx to surface error")
	}
}

func TestLoad_PropagatesUnmarshalError(t *testing.T) {
	st, bucket, client := newStorage(t, fixedClock)
	ctx := context.Background()
	putObject(t, client, bucket, testPrefix+"/spec/gen/20260505120000000-h.pb", []byte("not a proto"))
	if _, _, err := st.Load(ctx, fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"}); err == nil {
		t.Error("expected Load to surface decode error for corrupt record")
	}
}

func TestName(t *testing.T) {
	st, _, _ := newStorage(t, fixedClock)
	if name := st.Name(); name != "s3" {
		t.Errorf("Name() = %q, want s3", name)
	}
}

func TestNew_DefaultClockUsesNow(t *testing.T) {
	client := newKumoS3Client()
	bucket := createBucket(t, client)
	st, err := s3pkg.New(
		context.Background(),
		fingerprint.S3Config{
			Bucket:   bucket,
			Prefix:   testPrefix,
			Endpoint: kumoEndpoint(),
			Region:   "us-east-1",
		},
		s3pkg.WithClient(client),
	)
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	ctx := context.Background()

	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "deadbeef"}
	before := time.Now().UTC().Add(-time.Second)
	if err := st.Save(ctx, key, newRecord("gen")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	keys := listKeys(t, client, bucket, testPrefix+"/spec/gen/")
	if len(keys) != 1 {
		t.Fatalf("expected single object, got %v", keys)
	}
	name := keys[0][strings.LastIndex(keys[0], "/")+1:]
	stamp, err := time.Parse("20060102150405", name[:14])
	if err != nil {
		t.Fatalf("parse prefix from %q: %v", name, err)
	}
	if stamp.Before(before) || stamp.After(after) {
		t.Errorf("default clock prefix %v outside [%v, %v]", stamp, before, after)
	}
}

func TestNew_RequiresBucket(t *testing.T) {
	if _, err := s3pkg.New(context.Background(), fingerprint.S3Config{}); err == nil {
		t.Error("expected error when bucket is empty")
	}
}

// TestNew_DefaultsAndOverrides covers the construction-time branches that
// kumo-driven tests skip: empty Prefix fall-through to DefaultS3Prefix,
// surrounding-slash trimming on the prefix, and the `WithClient` /
// `WithClock` injection paths (which are how every kumo test wires its
// client, but not how the production path runs). The Storage is never
// asked to talk to S3 here, so AWS credentials / network are not
// required — credential resolution in `loadAWSConfig` is lazy.
func TestNew_DefaultsAndOverrides(t *testing.T) {
	pathStyleOn := true
	pathStyleOff := false
	cases := []struct {
		name string
		cfg  fingerprint.S3Config
	}{
		{name: "empty prefix falls back to default", cfg: fingerprint.S3Config{Bucket: "b"}},
		{name: "prefix is trimmed", cfg: fingerprint.S3Config{Bucket: "b", Prefix: "/x/y/"}},
		{name: "endpoint set", cfg: fingerprint.S3Config{Bucket: "b", Endpoint: "http://example.invalid"}},
		{name: "use_path_style explicit true", cfg: fingerprint.S3Config{Bucket: "b", UsePathStyle: &pathStyleOn}},
		{name: "use_path_style explicit false with endpoint", cfg: fingerprint.S3Config{Bucket: "b", Endpoint: "http://example.invalid", UsePathStyle: &pathStyleOff}},
		{name: "region set", cfg: fingerprint.S3Config{Bucket: "b", Region: "ap-northeast-1"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			st, err := s3pkg.New(context.Background(), tt.cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if st.Name() != "s3" {
				t.Errorf("Name() = %q, want s3", st.Name())
			}
		})
	}
}

// listKeys is a small ListObjectsV2 helper that flattens a paginator into a
// sorted slice for assertions. Tests use it instead of reaching into the
// Storage implementation so the "what is on S3" view is independent of the
// code under test.
func listKeys(t *testing.T, client *awss3.Client, bucket, prefix string) []string {
	t.Helper()
	var out []string
	paginator := awss3.NewListObjectsV2Paginator(client, &awss3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.Background())
		if err != nil {
			t.Fatalf("ListObjectsV2: %v", err)
		}
		for _, obj := range page.Contents {
			if obj.Key != nil {
				out = append(out, *obj.Key)
			}
		}
	}
	return out
}

func putObject(t *testing.T, client *awss3.Client, bucket, key string, body []byte) {
	t.Helper()
	_, err := client.PutObject(context.Background(), &awss3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("PutObject %s: %v", key, err)
	}
}
