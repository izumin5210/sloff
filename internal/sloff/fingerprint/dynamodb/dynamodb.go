package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"golang.org/x/sync/errgroup"

	fingerprintv1 "github.com/izumin5210/sloff/internal/proto/sloff/fingerprint/v1"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
)

const backendName = "dynamodb"

// Limits imposed by the DynamoDB API.
const (
	batchGetMaxItems   = 100
	batchWriteMaxItems = 25
	bulkConcurrency    = 8
)

// Config carries the connection knobs the dynamodb backend needs. Mirrors the
// shape of `.sloff/config.yml#fingerprint.dynamodb` so the cmd layer can pass
// the user-supplied values straight through. Credentials are resolved via the
// aws-sdk-go-v2 default chain and never appear here.
type Config struct {
	// Table is the DynamoDB table name. Required. The table must already
	// exist with the schema documented in keys.go (pk=S, sk=S).
	Table string

	// Region overrides AWS region resolution. Empty means "use the SDK's
	// default chain" (AWS_REGION / shared config / etc).
	Region string

	// Endpoint overrides the API endpoint, useful for emulators (kumo /
	// LocalStack / DynamoDB Local). Empty means "let the SDK resolve".
	Endpoint string

	// ExpiresAfterDays, if positive, makes Save / SaveMany set an
	// `expires_at` attribute equal to now + N days. Combined with TTL
	// enabled on the table this is the only GC the backend needs. Zero
	// disables the attribute entirely (records are kept indefinitely).
	ExpiresAfterDays int
}

// Storage persists records in DynamoDB. Construct via New; never use the
// zero value.
type Storage struct {
	client    *dynamodb.Client
	creds     aws.CredentialsProvider
	table     string
	expiresIn time.Duration // zero means "never set TTL"
	clock     func() time.Time
}

// Option customises a Storage at construction time. Tests use these to
// inject deterministic clocks / pre-built clients.
type Option func(*Storage)

// WithClock injects the wall clock used to stamp `expires_at`. Defaults to
// time.Now().UTC(); only meaningful when ExpiresAfterDays > 0.
func WithClock(clock func() time.Time) Option {
	return func(s *Storage) { s.clock = clock }
}

// WithClient lets tests inject a pre-built *dynamodb.Client (typically one
// pointed at a local emulator with static credentials). Production code
// should always go through New so credential resolution comes from the
// aws-sdk-go-v2 default chain.
func WithClient(c *dynamodb.Client) Option {
	return func(s *Storage) { s.client = c }
}

// WithCredentialsProvider lets tests inject the provider Warm resolves. New
// sets this from the resolved aws.Config; the WithClient path leaves it nil
// (Warm then no-ops), so tests that want to exercise Warm pass it explicitly.
func WithCredentialsProvider(p aws.CredentialsProvider) Option {
	return func(s *Storage) { s.creds = p }
}

// New constructs a Storage from cfg. Credentials come from the
// aws-sdk-go-v2 default chain so the caller never threads them through;
// only the repo-committed knobs (table / region / endpoint / TTL) live in
// cfg. Use WithClient in tests to bypass credential resolution.
func New(ctx context.Context, cfg Config, opts ...Option) (*Storage, error) {
	if cfg.Table == "" {
		return nil, errors.New("fingerprint/dynamodb: table is required")
	}
	if cfg.ExpiresAfterDays < 0 {
		return nil, fmt.Errorf("fingerprint/dynamodb: expires_after_days must be >= 0, got %d", cfg.ExpiresAfterDays)
	}

	st := &Storage{
		table:     cfg.Table,
		expiresIn: time.Duration(cfg.ExpiresAfterDays) * 24 * time.Hour,
		clock:     func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(st)
	}

	if st.client == nil {
		awsCfg, err := loadAWSConfig(ctx, cfg)
		if err != nil {
			return nil, err
		}
		st.client = dynamodb.NewFromConfig(awsCfg, func(o *dynamodb.Options) {
			if cfg.Endpoint != "" {
				o.BaseEndpoint = aws.String(cfg.Endpoint)
			}
		})
		st.creds = awsCfg.Credentials
	}
	return st, nil
}

// Warm forces credential resolution (the aws-sdk-go-v2 default chain retrieves
// lazily on first request — with SSO/STS that is a multi-hundred-ms to
// multi-second round-trip). Callers kick this off in the background at run
// start so the first real request (the prefetch BatchGetItem) hits an already
// populated credential cache instead of paying the resolution latency on the
// critical path. Safe to call concurrently with real requests: the SDK's
// CredentialsCache serialises resolution and shares the result. A no-op when
// the client was injected via WithClient (tests / emulators).
func (s *Storage) Warm(ctx context.Context) error {
	if s.creds == nil {
		return nil
	}
	_, err := s.creds.Retrieve(ctx)
	return err
}

func loadAWSConfig(ctx context.Context, cfg Config) (aws.Config, error) {
	var loaders []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		loaders = append(loaders, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loaders...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("fingerprint/dynamodb: load aws config: %w", err)
	}
	return awsCfg, nil
}

// Name implements fingerprint.Storage.
func (s *Storage) Name() string { return backendName }

// Load implements fingerprint.Storage with a single GetItem (eventually
// consistent — strong consistency is unnecessary because fingerprint cache
// is self-healing and EC reads are half the price).
func (s *Storage) Load(ctx context.Context, key fingerprint.Key) (*fingerprintv1.Record, bool, error) {
	keyAttrs, err := marshalKey(key)
	if err != nil {
		return nil, false, err
	}
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key:       keyAttrs,
	})
	if err != nil {
		return nil, false, fmt.Errorf("fingerprint/dynamodb: get %+v: %w", key, err)
	}
	if out.Item == nil {
		return nil, false, nil
	}
	rec, err := recordFromAttrs(out.Item)
	if err != nil {
		// Superseded schema versions read as misses so the runner
		// regenerates them (ADR-0010), mirroring the local backend.
		// Corruption stays a hard error.
		if errors.Is(err, fingerprint.ErrUnsupportedSchemaVersion) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("fingerprint/dynamodb: decode %+v: %w", key, err)
	}
	return rec, true, nil
}

// Save implements fingerprint.Storage with a PutItem. Last-write-wins; same
// (Key) writes are wire-byte identical (deterministic Marshal) so concurrent
// writers don't lose data.
func (s *Storage) Save(ctx context.Context, key fingerprint.Key, rec *fingerprintv1.Record) error {
	attrs, err := s.itemAttrs(key, rec)
	if err != nil {
		return err
	}
	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      attrs,
	})
	if err != nil {
		return fmt.Errorf("fingerprint/dynamodb: put %+v: %w", key, err)
	}
	return nil
}

// Delete implements fingerprint.Storage. Missing keys are a no-op.
func (s *Storage) Delete(ctx context.Context, key fingerprint.Key) error {
	keyAttrs, err := marshalKey(key)
	if err != nil {
		return err
	}
	_, err = s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key:       keyAttrs,
	})
	if err != nil {
		return fmt.Errorf("fingerprint/dynamodb: delete %+v: %w", key, err)
	}
	return nil
}

// List implements fingerprint.Storage. Falls back to Scan when the caller
// has no SpecRelpath hint; otherwise issues Query against the partition.
// OlderThan is applied client-side because DynamoDB filter expressions
// don't reduce RCU consumption — filtering in code is simpler and matches
// the local backend's semantics.
func (s *Storage) List(ctx context.Context, filter fingerprint.ListFilter) ([]fingerprint.Key, error) {
	if filter.SpecRelpath != "" {
		return s.listByQuery(ctx, filter)
	}
	return s.listByScan(ctx, filter)
}

func (s *Storage) listByQuery(ctx context.Context, filter fingerprint.ListFilter) ([]fingerprint.Key, error) {
	exprName := map[string]string{"#pk": pkAttr}
	exprValue := map[string]ddbtypes.AttributeValue{
		":pk": &ddbtypes.AttributeValueMemberS{Value: filter.SpecRelpath},
	}
	keyExpr := "#pk = :pk"
	if filter.TaskID != "" {
		exprName["#sk"] = skAttr
		exprValue[":skp"] = &ddbtypes.AttributeValueMemberS{Value: sortKeyTaskPrefix(filter.TaskID)}
		keyExpr += " AND begins_with(#sk, :skp)"
	}

	var keys []fingerprint.Key
	paginator := dynamodb.NewQueryPaginator(s.client, &dynamodb.QueryInput{
		TableName:                 aws.String(s.table),
		KeyConditionExpression:    aws.String(keyExpr),
		ExpressionAttributeNames:  exprName,
		ExpressionAttributeValues: exprValue,
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("fingerprint/dynamodb: query %s: %w", filter.SpecRelpath, err)
		}
		for _, attrs := range page.Items {
			k, ok, err := keyFromAttrs(attrs, &filter)
			if err != nil {
				return nil, fmt.Errorf("fingerprint/dynamodb: query %s: %w", filter.SpecRelpath, err)
			}
			if ok {
				keys = append(keys, k)
			}
		}
	}
	return keys, nil
}

func (s *Storage) listByScan(ctx context.Context, filter fingerprint.ListFilter) ([]fingerprint.Key, error) {
	var keys []fingerprint.Key
	paginator := dynamodb.NewScanPaginator(s.client, &dynamodb.ScanInput{
		TableName: aws.String(s.table),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("fingerprint/dynamodb: scan: %w", err)
		}
		for _, attrs := range page.Items {
			k, ok, err := keyFromAttrs(attrs, &filter)
			if err != nil {
				return nil, fmt.Errorf("fingerprint/dynamodb: scan: %w", err)
			}
			if !ok {
				continue
			}
			if filter.TaskID != "" && k.TaskID != filter.TaskID {
				continue
			}
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// CollapseDuplicates is a no-op for DynamoDB: the per-item schema cannot
// produce duplicates because each (spec, task, input_hash) maps to exactly
// one item. The interface contract permits returning (0, nil) for
// single-SSoT backends.
func (s *Storage) CollapseDuplicates(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return 0, nil
}

// LoadMany implements fingerprint.Storage by issuing parallel BatchGetItem
// calls. UnprocessedKeys (returned when DynamoDB throttles a batch) are
// merged back into the queue and retried until the input set is exhausted.
func (s *Storage) LoadMany(ctx context.Context, keys []fingerprint.Key) (map[fingerprint.Key]*fingerprintv1.Record, error) {
	if len(keys) == 0 {
		return map[fingerprint.Key]*fingerprintv1.Record{}, nil
	}
	out := make(map[fingerprint.Key]*fingerprintv1.Record, len(keys))
	var mu sync.Mutex

	// Index by composite identifier so a returned item can be mapped back
	// to the caller's original Key (preserves the SpecRelpath / TaskID /
	// InputHash split that DynamoDB serialises into the composite sort
	// key).
	keyByPair := make(map[[2]string]fingerprint.Key, len(keys))
	for _, k := range keys {
		keyByPair[[2]string{k.SpecRelpath, sortKey(k)}] = k
	}

	batches := chunkKeys(keys, batchGetMaxItems)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(bulkConcurrency)
	for _, batch := range batches {
		g.Go(func() error {
			pending := batch
			for len(pending) > 0 {
				if err := gctx.Err(); err != nil {
					return err
				}
				reqKeys := make([]map[string]ddbtypes.AttributeValue, 0, len(pending))
				for _, k := range pending {
					attrs, err := marshalKey(k)
					if err != nil {
						return err
					}
					reqKeys = append(reqKeys, attrs)
				}
				resp, err := s.client.BatchGetItem(gctx, &dynamodb.BatchGetItemInput{
					RequestItems: map[string]ddbtypes.KeysAndAttributes{
						s.table: {Keys: reqKeys},
					},
				})
				if err != nil {
					return fmt.Errorf("fingerprint/dynamodb: batch get: %w", err)
				}
				for _, attrs := range resp.Responses[s.table] {
					it, err := itemFromAttrs(attrs)
					if err != nil {
						return fmt.Errorf("fingerprint/dynamodb: decode item: %w", err)
					}
					k, ok := keyByPair[[2]string{it.PK, it.SK}]
					if !ok {
						continue
					}
					rec, err := fingerprint.Unmarshal(it.Record)
					if err != nil {
						// A superseded record is a per-item miss, not
						// a batch failure — failing here would abort
						// the run for every task via the prefetch
						// (ADR-0010).
						if errors.Is(err, fingerprint.ErrUnsupportedSchemaVersion) {
							continue
						}
						return fmt.Errorf("fingerprint/dynamodb: decode %+v: %w", k, err)
					}
					mu.Lock()
					out[k] = rec
					mu.Unlock()
				}
				pending = unprocessedKeys(resp.UnprocessedKeys[s.table], keyByPair)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}

// SaveMany implements fingerprint.Storage by issuing parallel
// BatchWriteItem calls. UnprocessedItems are retried in the same fashion
// as UnprocessedKeys for LoadMany.
func (s *Storage) SaveMany(ctx context.Context, items []fingerprint.KeyRecord) error {
	if len(items) == 0 {
		return nil
	}
	batches := chunkItems(items, batchWriteMaxItems)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(bulkConcurrency)
	for _, batch := range batches {
		g.Go(func() error {
			writes, err := s.encodeBatchWrites(batch)
			if err != nil {
				return err
			}
			pending := writes
			for len(pending) > 0 {
				if err := gctx.Err(); err != nil {
					return err
				}
				resp, err := s.client.BatchWriteItem(gctx, &dynamodb.BatchWriteItemInput{
					RequestItems: map[string][]ddbtypes.WriteRequest{s.table: pending},
				})
				if err != nil {
					return fmt.Errorf("fingerprint/dynamodb: batch write: %w", err)
				}
				pending = resp.UnprocessedItems[s.table]
			}
			return nil
		})
	}
	return g.Wait()
}

// itemAttrs builds the attribute map a PutItem expects by marshalling the
// typed item struct. The schema (which fields are written, with what
// types, under which attribute names) lives entirely on the struct tags
// in keys.go — there is no parallel layout description to keep in sync.
func (s *Storage) itemAttrs(key fingerprint.Key, rec *fingerprintv1.Record) (map[string]ddbtypes.AttributeValue, error) {
	body, err := fingerprint.Marshal(rec)
	if err != nil {
		return nil, err
	}
	now := s.clock()
	it := item{
		PK:        key.SpecRelpath,
		SK:        sortKey(key),
		Record:    body,
		CreatedAt: now,
	}
	if s.expiresIn > 0 {
		exp := now.Add(s.expiresIn)
		it.ExpiresAt = &exp
	}
	return attributevalue.MarshalMap(it)
}

func (s *Storage) encodeBatchWrites(batch []fingerprint.KeyRecord) ([]ddbtypes.WriteRequest, error) {
	out := make([]ddbtypes.WriteRequest, 0, len(batch))
	for _, kr := range batch {
		attrs, err := s.itemAttrs(kr.Key, kr.Record)
		if err != nil {
			return nil, err
		}
		out = append(out, ddbtypes.WriteRequest{
			PutRequest: &ddbtypes.PutRequest{Item: attrs},
		})
	}
	return out, nil
}

// marshalKey produces the {pk, sk} attribute map a GetItem / DeleteItem /
// BatchGetItem call needs. Mirrors itemAttrs in going through
// attributevalue + a typed struct so the attribute names live in one
// place (the dynamodbav tags on primaryKey).
func marshalKey(k fingerprint.Key) (map[string]ddbtypes.AttributeValue, error) {
	return attributevalue.MarshalMap(newPrimaryKey(k))
}

// itemFromAttrs decodes a returned attribute map into the typed item.
// Foreign items missing pk / sk surface as zero values on the struct
// fields; callers check item.SK / parseSortKey before treating the
// result as a sloff record.
func itemFromAttrs(attrs map[string]ddbtypes.AttributeValue) (item, error) {
	var it item
	if err := attributevalue.UnmarshalMap(attrs, &it); err != nil {
		return item{}, fmt.Errorf("decode item: %w", err)
	}
	return it, nil
}

// recordFromAttrs unmarshals the typed item and decodes its Record proto
// bytes. Returns an error if the attribute map is foreign (no record
// bytes) or the proto bytes are malformed.
func recordFromAttrs(attrs map[string]ddbtypes.AttributeValue) (*fingerprintv1.Record, error) {
	it, err := itemFromAttrs(attrs)
	if err != nil {
		return nil, err
	}
	if len(it.Record) == 0 {
		return nil, fmt.Errorf("missing %s attribute", recordAttr)
	}
	return fingerprint.Unmarshal(it.Record)
}

// keyFromAttrs reconstructs a fingerprint.Key from a stored item. Used by
// List / Scan to filter foreign items and report only well-formed records.
// The OlderThan filter is applied against item.CreatedAt (record write
// time, mirroring the local backend's mtime semantics); items without a
// CreatedAt are conservatively kept so list-based GC sweeps don't drop
// legacy entries written before this attribute was introduced.
func keyFromAttrs(attrs map[string]ddbtypes.AttributeValue, filter *fingerprint.ListFilter) (fingerprint.Key, bool, error) {
	it, err := itemFromAttrs(attrs)
	if err != nil {
		return fingerprint.Key{}, false, err
	}
	key, ok := it.toKey()
	if !ok {
		return fingerprint.Key{}, false, nil
	}
	if !filter.OlderThan.IsZero() && !it.CreatedAt.IsZero() {
		// "older than" semantics: skip items whose write time is at or
		// after the cutoff (i.e. recent enough to keep).
		if !it.CreatedAt.Before(filter.OlderThan) {
			return fingerprint.Key{}, false, nil
		}
	}
	return key, true, nil
}

// chunkKeys / chunkItems split slices into DynamoDB-batch-sized pieces.
func chunkKeys(in []fingerprint.Key, size int) [][]fingerprint.Key {
	if size <= 0 {
		return [][]fingerprint.Key{in}
	}
	var out [][]fingerprint.Key
	for i := 0; i < len(in); i += size {
		end := min(i+size, len(in))
		out = append(out, in[i:end])
	}
	return out
}

func chunkItems(in []fingerprint.KeyRecord, size int) [][]fingerprint.KeyRecord {
	if size <= 0 {
		return [][]fingerprint.KeyRecord{in}
	}
	var out [][]fingerprint.KeyRecord
	for i := 0; i < len(in); i += size {
		end := min(i+size, len(in))
		out = append(out, in[i:end])
	}
	return out
}

// unprocessedKeys converts a KeysAndAttributes block returned in
// UnprocessedKeys back into the fingerprint.Key list the next iteration of
// the BatchGetItem loop needs.
func unprocessedKeys(left ddbtypes.KeysAndAttributes, byPair map[[2]string]fingerprint.Key) []fingerprint.Key {
	var out []fingerprint.Key
	for _, raw := range left.Keys {
		var pk primaryKey
		if err := attributevalue.UnmarshalMap(raw, &pk); err != nil {
			continue
		}
		if pk.PK == "" || pk.SK == "" {
			continue
		}
		if k, ok := byPair[[2]string{pk.PK, pk.SK}]; ok {
			out = append(out, k)
		}
	}
	return out
}
