// Package s3 is an opt-in fingerprint.Storage backend that persists records
// in an S3-compatible object store. Used when a repo's .sloff/config.yml
// declares `fingerprint.backend: s3`. The wire format is identical to the
// local backend (proto binary, ADR-0009) and the object-key layout mirrors
// ADR-0010 so records can be moved between backends with `aws s3 cp`.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	fingerprintv1 "github.com/izumin5210/sloff/internal/proto/sloff/fingerprint/v1"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
)

const backendName = "s3"

// Storage is the S3-backed fingerprint.Storage implementation. The set of
// connection knobs intentionally matches the on-disk config schema 1:1 so
// callers do not have to maintain a translation layer.
type Storage struct {
	client *s3.Client
	bucket string
	prefix string
	clock  func() time.Time
}

// Option customises a Storage at construction time.
type Option func(*Storage)

// WithClock injects the wall clock used to stamp record filenames. Defaults
// to time.Now().UTC(); tests inject a fixed clock so generated object keys
// are deterministic.
func WithClock(clock func() time.Time) Option {
	return func(s *Storage) { s.clock = clock }
}

// WithClient lets tests inject a pre-configured *s3.Client (e.g. one pointed
// at a kumo emulator) instead of relying on New's aws-sdk-go-v2 default
// chain. Production code should always go through New so credentials, region
// resolution, and endpoint overrides come from the standard AWS sources.
func WithClient(client *s3.Client) Option {
	return func(s *Storage) { s.client = client }
}

// New constructs a Storage from the parsed S3Config. Credentials and any
// ambient endpoint / region come from aws-sdk-go-v2's default chain so the
// caller never needs to thread them through; only the repo-committed knobs
// (bucket / prefix / region / endpoint / use_path_style) are honoured here.
//
// Endpoint precedence: cfg.Endpoint > AWS_ENDPOINT_URL_S3 / AWS_ENDPOINT_URL
// (resolved by the SDK when cfg.Endpoint is empty). Path-style addressing is
// auto-enabled when cfg.Endpoint is set so emulators that only speak path
// style (kumo, MinIO, LocalStack) work without extra config; cfg.UsePathStyle
// can override either way.
func New(ctx context.Context, cfg fingerprint.S3Config, opts ...Option) (*Storage, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("fingerprint/s3: bucket is required")
	}

	awsCfg, err := loadAWSConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = resolvePathStyle(cfg)
	})

	prefix := cfg.Prefix
	if prefix == "" {
		prefix = fingerprint.DefaultS3Prefix
	}

	st := &Storage{
		client: client,
		bucket: cfg.Bucket,
		prefix: strings.Trim(prefix, "/"),
		clock:  func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(st)
	}
	return st, nil
}

func loadAWSConfig(ctx context.Context, cfg fingerprint.S3Config) (aws.Config, error) {
	var loaders []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		loaders = append(loaders, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loaders...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("fingerprint/s3: load aws config: %w", err)
	}
	return awsCfg, nil
}

// resolvePathStyle implements the documented default: path-style is on
// whenever an endpoint override is set (so emulators work out of the box) and
// off otherwise. Either default can be flipped via cfg.UsePathStyle.
func resolvePathStyle(cfg fingerprint.S3Config) bool {
	if cfg.UsePathStyle != nil {
		return *cfg.UsePathStyle
	}
	return cfg.Endpoint != ""
}

// Name implements fingerprint.Storage.
func (s *Storage) Name() string { return backendName }

// Load implements fingerprint.Storage. Returns the latest-timestamp record
// for the Key. Equivalent to local.Load: when a merge / parallel first-write
// has produced multiple timestamp variants, the deterministic-generator
// scope makes them semantically equivalent (ADR-0010), so latest is an
// arbitrary but well-defined choice.
func (s *Storage) Load(ctx context.Context, key fingerprint.Key) (*fingerprintv1.Record, bool, error) {
	matches, err := s.matchingObjects(ctx, key)
	if err != nil {
		return nil, false, err
	}
	if len(matches) == 0 {
		return nil, false, nil
	}
	latest := matches[len(matches)-1]
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(latest),
	})
	if err != nil {
		return nil, false, fmt.Errorf("fingerprint/s3: get %s: %w", latest, err)
	}
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, false, fmt.Errorf("fingerprint/s3: read %s: %w", latest, err)
	}
	rec, err := fingerprint.Unmarshal(body)
	if err != nil {
		return nil, false, fmt.Errorf("fingerprint/s3: decode %s: %w", latest, err)
	}
	return rec, true, nil
}

// Save implements fingerprint.Storage. Mirrors local.Save: 0 existing
// variants → new timestamped object; 1+ variants → put onto the
// earliest-prefix one and remove the rest (duplicate collapse, ADR-0010).
func (s *Storage) Save(ctx context.Context, key fingerprint.Key, record *fingerprintv1.Record) error {
	matches, err := s.matchingObjects(ctx, key)
	if err != nil {
		return err
	}
	body, err := fingerprint.Marshal(record)
	if err != nil {
		return err
	}

	var target string
	if len(matches) == 0 {
		target = objectKey(s.prefix, key, formatNow(s.clock()))
	} else {
		target = matches[0]
		if err := s.deleteAll(ctx, matches[1:]); err != nil {
			return err
		}
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(target),
		Body:   bytes.NewReader(body),
	})
	if err != nil {
		return fmt.Errorf("fingerprint/s3: put %s: %w", target, err)
	}
	return nil
}

// Delete implements fingerprint.Storage. Removes every timestamp variant
// matching the Key (mirroring local.Delete which sweeps every
// `<TS>-<hash>.pb` sibling for the same input_hash).
func (s *Storage) Delete(ctx context.Context, key fingerprint.Key) error {
	matches, err := s.matchingObjects(ctx, key)
	if err != nil {
		return err
	}
	return s.deleteAll(ctx, matches)
}

// List implements fingerprint.Storage. Walks the entire prefix (or just the
// scoped sub-prefix when filter.SpecRelpath / TaskID is set), folding
// duplicate timestamp variants into a single Key. The effective time used
// for OlderThan is the maximum LastModified across the duplicates because GC
// cares about the most recent activity, not the oldest (matches the local
// backend's "latest mtime wins" stance).
func (s *Storage) List(ctx context.Context, filter fingerprint.ListFilter) ([]fingerprint.Key, error) {
	type bucket struct{ latest time.Time }
	seen := make(map[fingerprint.Key]*bucket)

	listPrefix := s.listPrefixForFilter(filter)
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(listPrefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("fingerprint/s3: list %s: %w", listPrefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			spec, task, hash, _, ok := parseObjectKey(s.prefix, *obj.Key)
			if !ok {
				continue
			}
			k := fingerprint.Key{SpecRelpath: spec, TaskID: task, InputHash: hash}
			if filter.SpecRelpath != "" && k.SpecRelpath != filter.SpecRelpath {
				continue
			}
			if filter.TaskID != "" && k.TaskID != filter.TaskID {
				continue
			}
			lm := time.Time{}
			if obj.LastModified != nil {
				lm = *obj.LastModified
			}
			if b, found := seen[k]; found {
				if lm.After(b.latest) {
					b.latest = lm
				}
			} else {
				seen[k] = &bucket{latest: lm}
			}
		}
	}

	keys := make([]fingerprint.Key, 0, len(seen))
	for k, b := range seen {
		if !filter.OlderThan.IsZero() && !b.latest.Before(filter.OlderThan) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].SpecRelpath != keys[j].SpecRelpath {
			return keys[i].SpecRelpath < keys[j].SpecRelpath
		}
		if keys[i].TaskID != keys[j].TaskID {
			return keys[i].TaskID < keys[j].TaskID
		}
		return keys[i].InputHash < keys[j].InputHash
	})
	return keys, nil
}

// CollapseDuplicates implements fingerprint.Storage by sweeping every Key
// that has more than one timestamp variant down to the earliest, mirroring
// local.CollapseDuplicates. In a single-SSoT backend this is rare (only
// triggered when two writers race on a first-write for the same input_hash),
// but the safety net is symmetric with local so `sloff fingerprint gc` works
// regardless of which backend a repo selects.
func (s *Storage) CollapseDuplicates(ctx context.Context) (int, error) {
	keys, err := s.List(ctx, fingerprint.ListFilter{})
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		matches, err := s.matchingObjects(ctx, k)
		if err != nil {
			return removed, err
		}
		if len(matches) <= 1 {
			continue
		}
		if err := s.deleteAll(ctx, matches[1:]); err != nil {
			return removed, err
		}
		removed += len(matches) - 1
	}
	return removed, nil
}

// matchingObjects enumerates `<TS>-<key.InputHash>.pb` keys under the Key's
// directory prefix, sorted ascending by full key (= chronologically by the
// initial-creation timestamp). Keys whose filename does not have the
// timestamp shape are ignored, mirroring local.matchingFiles.
func (s *Storage) matchingObjects(ctx context.Context, key fingerprint.Key) ([]string, error) {
	prefix := objectPrefix(s.prefix, key)
	suffix := suffixForHash(key.InputHash)

	var out []string
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			if isNoSuchBucket(err) {
				return nil, fmt.Errorf("fingerprint/s3: bucket %q does not exist: %w", s.bucket, err)
			}
			return nil, fmt.Errorf("fingerprint/s3: list %s: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			k := *obj.Key
			if !strings.HasSuffix(k, suffix) {
				continue
			}
			filename := k[strings.LastIndex(k, "/")+1:]
			ts := strings.TrimSuffix(filename, suffix)
			if !looksLikeTimestamp(ts) {
				continue
			}
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *Storage) listPrefixForFilter(filter fingerprint.ListFilter) string {
	root := rootPrefix(s.prefix)
	if filter.SpecRelpath == "" {
		return root
	}
	scoped := root + strings.Trim(filter.SpecRelpath, "/") + "/"
	if filter.TaskID == "" {
		return scoped
	}
	return scoped + filter.TaskID + "/"
}

func (s *Storage) deleteAll(ctx context.Context, keys []string) error {
	for _, k := range keys {
		_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(k),
		})
		if err != nil && !isNotFound(err) {
			return fmt.Errorf("fingerprint/s3: delete %s: %w", k, err)
		}
	}
	return nil
}

// formatNow renders the supplied UTC timestamp into the ADR-0010 prefix
// shape. Always coerced to UTC so the prefix is comparable across developers
// in different timezones.
func formatNow(t time.Time) string {
	t = t.UTC()
	return formatTimestamp(
		t.Year(), int(t.Month()), t.Day(),
		t.Hour(), t.Minute(), t.Second(),
		t.Nanosecond()/int(time.Millisecond),
	)
}

// isNotFound matches both "object missing" (NoSuchKey, sometimes surfaced as
// a 404 with no typed shape) and non-typed 404s coming from S3-compatible
// emulators that don't always populate smithy's typed errors.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}
	return false
}

func isNoSuchBucket(err error) bool {
	if err == nil {
		return false
	}
	var nsb *s3types.NoSuchBucket
	if errors.As(err, &nsb) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode() == "NoSuchBucket"
	}
	return false
}
