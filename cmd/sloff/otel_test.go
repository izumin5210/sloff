package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"go.opentelemetry.io/otel"
)

// clearOTelEnv unsets every OTEL_*/SLOFF_OTEL_* key that the package consults so the
// caller's shell does not leak into the test. t.Setenv records the original value, so
// the subsequent os.Unsetenv leaves a snapshot the test cleanup restores afterwards.
func clearOTelEnv(t *testing.T) {
	t.Helper()
	for _, k := range otelEnvKeys {
		t.Setenv(k, "")
		os.Unsetenv(k)
		sloffKey := "SLOFF_" + k
		t.Setenv(sloffKey, "")
		os.Unsetenv(sloffKey)
	}
}

func TestEffectiveEnv(t *testing.T) {
	t.Run("SLOFF_ value wins over OTEL_", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://shell:4318")
		t.Setenv("SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT", "http://sloff:4318")
		if got := effectiveEnv("OTEL_EXPORTER_OTLP_ENDPOINT"); got != "http://sloff:4318" {
			t.Fatalf("effectiveEnv = %q, want SLOFF_ value", got)
		}
	})

	t.Run("falls back to OTEL_ when SLOFF_ unset", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://shell:4318")
		if got := effectiveEnv("OTEL_EXPORTER_OTLP_ENDPOINT"); got != "http://shell:4318" {
			t.Fatalf("effectiveEnv = %q, want shell value", got)
		}
	})

	t.Run("explicit SLOFF_=\"\" blanks the effective value", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
		t.Setenv("SLOFF_OTEL_TRACES_EXPORTER", "")
		if got := effectiveEnv("OTEL_TRACES_EXPORTER"); got != "" {
			t.Fatalf("effectiveEnv = %q, want \"\" (SLOFF_ explicit empty)", got)
		}
	})
}

func TestEnvOTelEnabled(t *testing.T) {
	cases := []struct {
		name string
		set  map[string]string
		want bool
	}{
		{name: "all unset", set: nil, want: false},
		{name: "OTLP endpoint set", set: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318"}, want: true},
		{name: "OTLP traces endpoint set", set: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://localhost:4318/v1/traces"}, want: true},
		{name: "traces exporter set to otlp", set: map[string]string{"OTEL_TRACES_EXPORTER": "otlp"}, want: true},
		{name: "traces exporter explicitly none", set: map[string]string{"OTEL_TRACES_EXPORTER": "none"}, want: false},
		{name: "traces exporter none case-insensitive", set: map[string]string{"OTEL_TRACES_EXPORTER": "NONE"}, want: false},
		{
			name: "endpoint set but SDK disabled wins",
			set:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318", "OTEL_SDK_DISABLED": "true"},
			want: false,
		},
		{
			name: "SDK disabled case-insensitive",
			set:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318", "OTEL_SDK_DISABLED": "TRUE"},
			want: false,
		},
		{
			name: "SDK disabled false does not block enable",
			set:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318", "OTEL_SDK_DISABLED": "false"},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearOTelEnv(t)
			for k, v := range tc.set {
				t.Setenv(k, v)
			}
			if got := envOTelEnabled(); got != tc.want {
				t.Fatalf("envOTelEnabled() = %v, want %v (env=%v)", got, tc.want, tc.set)
			}
		})
	}
}

// TestEnvOTelEnabled_SloffPrefixIntegration checks that envOTelEnabled honors
// SLOFF_-prefix overrides via effectiveEnv without mutating process env (so
// subprocesses spawned later by sloff never inherit SLOFF_-derived OTEL_*).
func TestEnvOTelEnabled_SloffPrefixIntegration(t *testing.T) {
	t.Run("SLOFF_OTEL_TRACES_EXPORTER=none silences sloff while shell is otlp", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://shell:4318")
		t.Setenv("SLOFF_OTEL_TRACES_EXPORTER", "none")
		if envOTelEnabled() {
			t.Fatal("envOTelEnabled() = true, want false (SLOFF_ silence wins)")
		}
		if got := os.Getenv("OTEL_TRACES_EXPORTER"); got != "" {
			t.Fatalf("envOTelEnabled mutated OTEL_TRACES_EXPORTER = %q, want untouched", got)
		}
	})

	t.Run("SLOFF_OTEL_SDK_DISABLED=true silences sloff", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://shell:4318")
		t.Setenv("SLOFF_OTEL_SDK_DISABLED", "true")
		if envOTelEnabled() {
			t.Fatal("envOTelEnabled() = true, want false")
		}
	})

	t.Run("SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT enables sloff when shell is empty", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT", "http://sloff-only:4318")
		if !envOTelEnabled() {
			t.Fatal("envOTelEnabled() = false, want true")
		}
		if _, ok := os.LookupEnv("OTEL_EXPORTER_OTLP_ENDPOINT"); ok {
			t.Fatal("envOTelEnabled leaked SLOFF_ value into OTEL_*; want env untouched")
		}
	})
}

func TestParseOTLPHeaders(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr bool
	}{
		{name: "empty", in: "", want: nil},
		{name: "single pair", in: "x-api-key=abc123", want: map[string]string{"x-api-key": "abc123"}},
		{name: "multiple pairs", in: "k1=v1,k2=v2", want: map[string]string{"k1": "v1", "k2": "v2"}},
		{name: "trims whitespace", in: " k1 = v1 , k2=v2 ", want: map[string]string{"k1": "v1", "k2": "v2"}},
		{name: "url-decoded value", in: "auth=Bearer%20token", want: map[string]string{"auth": "Bearer token"}},
		{name: "value containing equals", in: "k1=a=b", want: map[string]string{"k1": "a=b"}},
		{name: "missing equals errors", in: "lone-key", wantErr: true},
		{name: "empty key errors", in: "=value", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOTLPHeaders(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil (got=%v)", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseOTLPHeaders(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseResourceAttributes(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		got := parseResourceAttributes("env=prod,region=us-west")
		gotMap := map[string]string{}
		for _, kv := range got {
			gotMap[string(kv.Key)] = kv.Value.AsString()
		}
		want := map[string]string{"env": "prod", "region": "us-west"}
		if !reflect.DeepEqual(gotMap, want) {
			t.Fatalf("got %v, want %v", gotMap, want)
		}
	})

	t.Run("malformed pairs are skipped silently", func(t *testing.T) {
		got := parseResourceAttributes("env=prod,malformed,k=v")
		if len(got) != 2 {
			t.Fatalf("got %d attrs, want 2 (malformed skipped)", len(got))
		}
	})

	t.Run("url-decoded values", func(t *testing.T) {
		got := parseResourceAttributes("desc=hello%20world")
		if len(got) != 1 || got[0].Value.AsString() != "hello world" {
			t.Fatalf("url decoding broken: %v", got)
		}
	})

	t.Run("empty input returns nil", func(t *testing.T) {
		if got := parseResourceAttributes(""); got != nil {
			t.Fatalf("expected nil for empty input, got %v", got)
		}
	})
}

func TestBuildSpanExporter_Dispatch(t *testing.T) {
	t.Run("console exporter", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_TRACES_EXPORTER", "console")
		exp, err := buildSpanExporter(context.Background())
		if err != nil {
			t.Fatalf("buildSpanExporter err = %v", err)
		}
		// console (stdouttrace) supports Shutdown without network I/O.
		if err := exp.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown stdout exporter: %v", err)
		}
	})

	t.Run("default (otlp http)", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
		exp, err := buildSpanExporter(context.Background())
		if err != nil {
			t.Fatalf("buildSpanExporter err = %v", err)
		}
		_ = exp.Shutdown(context.Background())
	})

	t.Run("explicit otlp grpc", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4317")
		t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
		exp, err := buildSpanExporter(context.Background())
		if err != nil {
			t.Fatalf("buildSpanExporter err = %v", err)
		}
		_ = exp.Shutdown(context.Background())
	})

	t.Run("unsupported exporter rejected", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_TRACES_EXPORTER", "zipkin")
		_, err := buildSpanExporter(context.Background())
		if err == nil {
			t.Fatal("expected error for unsupported exporter, got nil")
		}
	})

	t.Run("unsupported protocol rejected", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
		t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "carrier-pigeon")
		_, err := buildSpanExporter(context.Background())
		if err == nil {
			t.Fatal("expected error for unsupported protocol, got nil")
		}
	})
}

func TestSetupTracing_DisabledIsZeroCost(t *testing.T) {
	clearOTelEnv(t)
	ctx := context.Background()

	shutdown, err := setupTracing(ctx)
	if err != nil {
		t.Fatalf("setupTracing returned err = %v, want nil when env is empty", err)
	}
	if shutdown == nil {
		t.Fatal("setupTracing returned nil shutdown; expected callable no-op")
	}
	if err := shutdown(ctx); err != nil {
		t.Fatalf("noop shutdown returned err = %v, want nil", err)
	}
}

func TestSetupTracing_EnabledReturnsRealShutdown(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "console")
	ctx := context.Background()

	shutdown, err := setupTracing(ctx)
	if err != nil {
		t.Fatalf("setupTracing returned err = %v", err)
	}
	if shutdown == nil {
		t.Fatal("setupTracing returned nil shutdown")
	}
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown returned err = %v", err)
	}
}

// TestSetupTracing_NeverMutatesEnv asserts the strongest contract: across
// disabled, enabled, and SLOFF_-overriding scenarios the OTEL_* env vars are
// never written to. Subprocesses spawned by runner.Run inherit os.Environ()
// verbatim, so any mutation here would leak SLOFF_-derived values into
// codegen tools — the bug codex flagged and we now structurally prevent.
func TestSetupTracing_NeverMutatesEnv(t *testing.T) {
	scenarios := []struct {
		name   string
		setEnv map[string]string
	}{
		{
			name:   "disabled (no env)",
			setEnv: nil,
		},
		{
			name: "disabled with SLOFF_ overrides",
			setEnv: map[string]string{
				"SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT": "http://sloff-only:4318",
				"SLOFF_OTEL_TRACES_EXPORTER":        "none",
			},
		},
		{
			name: "enabled via shell OTEL_*",
			setEnv: map[string]string{
				"OTEL_TRACES_EXPORTER":        "console",
				"OTEL_EXPORTER_OTLP_ENDPOINT": "http://shell:4318",
			},
		},
		{
			name: "enabled via SLOFF_* with shell OTEL_* shadow",
			setEnv: map[string]string{
				"OTEL_TRACES_EXPORTER":              "console",
				"OTEL_EXPORTER_OTLP_ENDPOINT":       "http://shell:4318",
				"SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT": "http://sloff:9999",
				"SLOFF_OTEL_SERVICE_NAME":           "sloff-renamed",
			},
		},
	}

	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) {
			clearOTelEnv(t)
			for k, v := range tc.setEnv {
				t.Setenv(k, v)
			}
			before := snapshotOTELEnv()
			ctx := context.Background()

			shutdown, err := setupTracing(ctx)
			if err != nil {
				t.Fatalf("setupTracing err = %v", err)
			}
			during := snapshotOTELEnv()
			if !reflect.DeepEqual(before, during) {
				t.Fatalf("setupTracing mutated OTEL_* env\n before=%v\n during=%v", before, during)
			}

			if err := shutdown(ctx); err != nil {
				t.Fatalf("shutdown err = %v", err)
			}
			after := snapshotOTELEnv()
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("shutdown mutated OTEL_* env\n before=%v\n  after=%v", before, after)
			}
		})
	}
}

// snapshotOTELEnv records the (set, value) state of every key sloff might
// touch. Used to assert env immutability across setupTracing's lifecycle.
func snapshotOTELEnv() map[string]any {
	snap := map[string]any{}
	for _, k := range otelEnvKeys {
		if v, ok := os.LookupEnv(k); ok {
			snap[k] = v
		}
	}
	return snap
}

// TestSetupTracing_DisabledDoesNotClobberGlobal preserves the codex-flagged
// invariant: a disabled sloff invocation must leave a host-configured global
// TracerProvider in place.
func TestSetupTracing_DisabledDoesNotClobberGlobal(t *testing.T) {
	clearOTelEnv(t)
	prev := otel.GetTracerProvider()
	ctx := context.Background()

	shutdown, err := setupTracing(ctx)
	if err != nil {
		t.Fatalf("setupTracing returned err = %v", err)
	}
	t.Cleanup(func() { _ = shutdown(ctx) })

	if got := otel.GetTracerProvider(); got != prev {
		t.Fatal("disabled setupTracing replaced global TracerProvider; want unchanged")
	}
}

// TestSetupTracing_EnabledRestoresProviderAndPropagator covers the enabled-
// path mirror: sloff installs its own TracerProvider and propagator, but the
// shutdown must put the prior provider and propagator back so the host
// process is not stuck with sloff's shut-down provider after the command
// returns.
func TestSetupTracing_EnabledRestoresProviderAndPropagator(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "console")

	prevProvider := otel.GetTracerProvider()
	prevPropagator := otel.GetTextMapPropagator()
	ctx := context.Background()

	shutdown, err := setupTracing(ctx)
	if err != nil {
		t.Fatalf("setupTracing returned err = %v", err)
	}
	if got := otel.GetTracerProvider(); got == prevProvider {
		t.Fatal("enabled setupTracing did not install a new TracerProvider")
	}

	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown returned err = %v", err)
	}
	if got := otel.GetTracerProvider(); got != prevProvider {
		t.Fatal("post-shutdown TracerProvider not restored")
	}
	if got := otel.GetTextMapPropagator(); got != prevPropagator {
		t.Fatal("post-shutdown propagator not restored")
	}
}

// errSentinel is a sanity check that errors.Is is wired correctly when we
// fall through unsupported-exporter handling. Currently unused beyond a
// compile-time reference; kept so future tests can grep for the pattern.
var _ = errors.Is
