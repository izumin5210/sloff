package s3_test

// kumo is run via `go tool kumo` (declared in go.mod's tool block) so the
// suite needs no manually installed binary on developer machines or CI. The
// emulator is spawned once in TestMain on a random free port and shared by
// all tests; each test creates its own bucket to avoid cross-test
// interference.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

// kumoPort is the port the singleton kumo emulator listens on for the
// duration of `go test`. Pinned to 4566 because kumo (as of v0.18.2)
// hardcodes server.DefaultConfig{Host: "0.0.0.0", Port: 4566} and ignores
// both KUMO_PORT / KUMO_HOST env vars and --port / --host flags despite
// what its README and `--help` text claim. Pre-checked in startKumo so an
// already-occupied 4566 fails with a clear error rather than a confusing
// AWS SDK timeout.
const kumoPort = 4566

// startKumoOnce guards the spawn so concurrent invocations from package init
// or accidental TestMain reentry do not race on os/exec.
var startKumoOnce sync.Once

// kumoCmd holds the kumo subprocess so TestMain's defer can shut it down.
// The shutdown path is best-effort: tests should pass or fail on their own
// merits, not because we leaked a kumo process.
var kumoCmd *exec.Cmd

func TestMain(m *testing.M) {
	if err := startKumo(); err != nil {
		fmt.Fprintf(os.Stderr, "fingerprint/s3: start kumo: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	stopKumo()
	os.Exit(code)
}

func startKumo() error {
	var startErr error
	startKumoOnce.Do(func() {
		// Bail out early if 4566 is already in use; otherwise the spawn
		// silently succeeds but the listener fails to bind and every
		// subsequent S3 call blows up with an opaque connection error.
		if l, err := net.Listen("tcp", kumoEndpointHostPort()); err != nil {
			startErr = fmt.Errorf("port %d already in use; free it before running this suite (kumo currently has no port flag): %w", kumoPort, err)
			return
		} else {
			_ = l.Close()
		}

		// `go tool kumo` resolves the tool-dependency binary declared in
		// go.mod. Falling back to a developer-installed kumo on PATH is
		// intentionally not supported so test runs are reproducible across
		// machines.
		repoRoot, err := findRepoRoot()
		if err != nil {
			startErr = fmt.Errorf("find repo root: %w", err)
			return
		}
		cmd := exec.Command("go", "tool", "kumo")
		cmd.Dir = repoRoot
		// kumo currently ignores KUMO_HOST / KUMO_PORT / KUMO_LOG_LEVEL so
		// these are just hints for whoever inspects the spawned process; the
		// real binding lives in kumo's hardcoded server.DefaultConfig.
		cmd.Env = append(
			os.Environ(),
			"KUMO_LOG_LEVEL=warn",
		)
		// Stream kumo output to the test runner only when kumo itself
		// misbehaves; routing to /dev/null in the happy path keeps `go test
		// -v` legible.
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		// Put kumo in its own process group so a panicking test that exits
		// without calling stopKumo does not leave a stray emulator behind on
		// CI runners that reuse workspaces (`pgid` kill below).
		cmd.SysProcAttr = newSysProcAttr()
		if err := cmd.Start(); err != nil {
			startErr = fmt.Errorf("start kumo: %w", err)
			return
		}
		kumoCmd = cmd

		if err := waitForKumoReady(); err != nil {
			stopKumo()
			startErr = fmt.Errorf("kumo readiness: %w", err)
			return
		}
	})
	return startErr
}

func stopKumo() {
	if kumoCmd == nil || kumoCmd.Process == nil {
		return
	}
	// Prefer process-group kill on unix so any kumo-spawned children also go
	// away. On platforms where SysProcAttr lacks Setpgid we fall back to a
	// regular Kill on the leader.
	if err := killGroup(kumoCmd); err != nil {
		_ = kumoCmd.Process.Kill()
	}
	_, _ = kumoCmd.Process.Wait()
}

// waitForKumoReady waits for kumo to bind its HTTP listener and then to
// accept an authenticated S3 request. The two stages are explicit so the
// failure message identifies which phase stalled (TCP bind vs S3 dispatch);
// kumo cold-starts in roughly 7s on a clean machine because every supported
// service is initialised eagerly, so the budget is generous on purpose.
func waitForKumoReady() error {
	const budget = 60 * time.Second
	deadline := time.Now().Add(budget)

	if err := waitForTCP(kumoEndpointHostPort(), deadline); err != nil {
		return fmt.Errorf("kumo TCP listener never came up: %w", err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	client := newKumoS3Client()
	var lastErr error
	for {
		_, err := client.ListBuckets(ctx, &awss3.ListBucketsInput{})
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return fmt.Errorf("kumo S3 dispatch never accepted: last err %v", lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func waitForTCP(addr string, deadline time.Time) error {
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("deadline reached")
}

func kumoEndpointHostPort() string {
	return "127.0.0.1:" + strconv.Itoa(kumoPort)
}

// newKumoS3Client builds an aws-sdk-go-v2 S3 client pointed at the local
// kumo emulator. Kept inside the test package because production code must
// never hardcode kumo's credentials or path-style flag.
func newKumoS3Client() *awss3.Client {
	cfg, err := awsconfig.LoadDefaultConfig(
		context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		panic(err)
	}
	return awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		o.BaseEndpoint = aws.String(kumoEndpoint())
		o.UsePathStyle = true
	})
}

func kumoEndpoint() string {
	return "http://" + kumoEndpointHostPort()
}

// findRepoRoot walks upward from the test binary's directory until it finds a
// go.mod, so `go tool kumo` resolves against the repo's tool block
// regardless of the package's nesting depth.
func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above " + wd)
		}
		dir = parent
	}
}

// createBucket creates a per-test bucket on kumo. The bucket name is built
// from the test name to make CI failures easier to diagnose; a small random
// nonce avoids collisions when a sub-test replays the same name.
func createBucket(t *testing.T, client *awss3.Client) string {
	t.Helper()
	name := bucketName(t)
	_, err := client.CreateBucket(context.Background(), &awss3.CreateBucketInput{
		Bucket: aws.String(name),
	})
	if err != nil {
		t.Fatalf("CreateBucket %q: %v", name, err)
	}
	t.Cleanup(func() { emptyAndDeleteBucket(t, client, name) })
	return name
}

func bucketName(t *testing.T) string {
	t.Helper()
	// S3 bucket names are 3-63 chars, lowercase, no underscores. Test names
	// can include slashes (sub-tests) and uppercase, so coerce.
	return sanitizeBucketName(t.Name()) + "-" + nonce()
}

func sanitizeBucketName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	if len(out) > 50 {
		out = out[:50]
	}
	return string(bytes.TrimLeft(bytes.TrimRight(out, "-"), "-"))
}

var nonceCounter struct {
	sync.Mutex
	n int
}

func nonce() string {
	nonceCounter.Lock()
	defer nonceCounter.Unlock()
	nonceCounter.n++
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), nonceCounter.n)
}

func emptyAndDeleteBucket(t *testing.T, client *awss3.Client, bucket string) {
	t.Helper()
	ctx := context.Background()
	paginator := awss3.NewListObjectsV2Paginator(client, &awss3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			t.Logf("teardown list %s: %v", bucket, err)
			return
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			_, _ = client.DeleteObject(ctx, &awss3.DeleteObjectInput{
				Bucket: aws.String(bucket),
				Key:    obj.Key,
			})
		}
	}
	_, _ = client.DeleteBucket(ctx, &awss3.DeleteBucketInput{Bucket: aws.String(bucket)})
}
