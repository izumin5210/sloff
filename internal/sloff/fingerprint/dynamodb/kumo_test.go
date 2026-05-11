package dynamodb_test

// Spawn one kumo emulator per `go test` invocation and share it across all
// tests. Pinned to port 4566 because kumo (v0.18.2) ignores KUMO_PORT /
// --port despite README claims; pre-binding 4566 fails fast if some other
// process is holding it.

import (
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
	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// kumoPort is the port the singleton kumo emulator listens on for the
// duration of `go test`. Pinned to 4566 because kumo (as of v0.18.2)
// hardcodes server.DefaultConfig{Host: "0.0.0.0", Port: 4566} and ignores
// both KUMO_PORT / KUMO_HOST env vars and --port / --host flags despite
// what its README and `--help` text claim. Tracked in Linear PD-20.
const kumoPort = 4566

var (
	startKumoOnce sync.Once
	kumoCmd       *exec.Cmd
)

func TestMain(m *testing.M) {
	if err := startKumo(); err != nil {
		fmt.Fprintf(os.Stderr, "fingerprint/dynamodb: start kumo: %v\n", err)
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
		// subsequent DynamoDB call blows up with an opaque connection
		// error.
		if l, err := net.Listen("tcp", kumoEndpointHostPort()); err != nil {
			startErr = fmt.Errorf("port %d already in use; free it before running this suite (kumo currently has no port flag): %w", kumoPort, err)
			return
		} else {
			_ = l.Close()
		}

		repoRoot, err := findRepoRoot()
		if err != nil {
			startErr = fmt.Errorf("find repo root: %w", err)
			return
		}
		cmd := exec.Command("go", "tool", "kumo")
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(), "KUMO_LOG_LEVEL=warn")
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
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
	if err := killGroup(kumoCmd); err != nil {
		_ = kumoCmd.Process.Kill()
	}
	_, _ = kumoCmd.Process.Wait()
}

func waitForKumoReady() error {
	const budget = 60 * time.Second
	deadline := time.Now().Add(budget)

	if err := waitForTCP(kumoEndpointHostPort(), deadline); err != nil {
		return fmt.Errorf("kumo TCP listener never came up: %w", err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	client := newKumoDDBClient()
	var lastErr error
	for {
		_, err := client.ListTables(ctx, &awsddb.ListTablesInput{})
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return fmt.Errorf("kumo DynamoDB dispatch never accepted: last err %v", lastErr)
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

func kumoEndpoint() string {
	return "http://" + kumoEndpointHostPort()
}

// newKumoDDBClient builds a DynamoDB client pointed at the local kumo
// emulator. Production code must never bake in kumo's static credentials;
// this helper lives in the test package only.
func newKumoDDBClient() *awsddb.Client {
	cfg, err := awsconfig.LoadDefaultConfig(
		context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		panic(err)
	}
	return awsddb.NewFromConfig(cfg, func(o *awsddb.Options) {
		o.BaseEndpoint = aws.String(kumoEndpoint())
	})
}

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

// createTable provisions a per-test DynamoDB table with the schema sloff
// expects (pk=S hash, sk=S range). The cleanup hook deletes it so each
// test runs in isolation.
func createTable(t *testing.T, client *awsddb.Client) string {
	t.Helper()
	name := tableName(t)
	_, err := client.CreateTable(context.Background(), &awsddb.CreateTableInput{
		TableName:   aws.String(name),
		BillingMode: ddbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("sk"), AttributeType: ddbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []ddbtypes.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: ddbtypes.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: ddbtypes.KeyTypeRange},
		},
	})
	if err != nil {
		t.Fatalf("CreateTable %q: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = client.DeleteTable(context.Background(), &awsddb.DeleteTableInput{TableName: aws.String(name)})
	})
	return name
}

func tableName(t *testing.T) string {
	t.Helper()
	return sanitize(t.Name()) + "-" + nonce()
}

func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	if len(out) > 100 {
		out = out[:100]
	}
	return string(out)
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
