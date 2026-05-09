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
		{name: "percent-decoded value", in: "auth=Bearer%20token", want: map[string]string{"auth": "Bearer token"}},
		// Regression: url.QueryUnescape rewrites '+' to space, which silently
		// corrupts base64-encoded auth tokens (e.g. Bearer "abc+def==" → "abc def==").
		// The header parser must use path-style decoding so '+' stays literal.
		{name: "literal plus preserved (base64-style auth)", in: "Authorization=Bearer abc+def/==", want: map[string]string{"Authorization": "Bearer abc+def/=="}},
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

	t.Run("percent-decoded values", func(t *testing.T) {
		got := parseResourceAttributes("desc=hello%20world")
		if len(got) != 1 || got[0].Value.AsString() != "hello world" {
			t.Fatalf("url decoding broken: %v", got)
		}
	})

	// Regression: url.QueryUnescape rewrites '+' to space, which would corrupt
	// resource attribute values that legitimately carry '+' (commit hashes,
	// version qualifiers like "1.0+local", base64-derived deployment ids, etc).
	t.Run("literal plus preserved", func(t *testing.T) {
		got := parseResourceAttributes("deployment.id=v1+local")
		if len(got) != 1 || got[0].Value.AsString() != "v1+local" {
			t.Fatalf("got %v, want literal '+' preserved", got)
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

	// http/json is in the OTel SDK config spec but otel-go's otlptracehttp
	// emits OTLP/protobuf bytes regardless. Silently sending protobuf when the
	// user asked for JSON breaks collectors that expect JSON, so reject the
	// value at startup with a clear message.
	t.Run("http/json rejected (otel-go SDK does not implement)", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
		t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/json")
		_, err := buildSpanExporter(context.Background())
		if err == nil {
			t.Fatal("expected error for http/json, got nil")
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

// TestResolveOTLPHTTPEndpoint locks in the OTel env-config rule that
// OTEL_EXPORTER_OTLP_ENDPOINT is a *base* URL with /v1/traces appended for
// the traces signal, while OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is taken
// as-is. Without this, a perfectly normal `http://collector:4318` setup
// would POST to "/" instead of "/v1/traces" and most collectors would
// reject the export.
func TestResolveOTLPHTTPEndpoint(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "no env returns empty (exporter uses its own default)",
			env:  nil,
			want: "",
		},
		{
			name: "generic appends /v1/traces",
			env:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"},
			want: "http://collector:4318/v1/traces",
		},
		{
			name: "generic strips trailing slash before appending",
			env:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318/"},
			want: "http://collector:4318/v1/traces",
		},
		{
			name: "signal-specific used as-is (not modified)",
			env:  map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "https://traces.example.com/api/v1/traces"},
			want: "https://traces.example.com/api/v1/traces",
		},
		{
			name: "signal-specific wins over generic",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT":        "http://generic:4318",
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://signal:4318/v1/traces",
			},
			want: "http://signal:4318/v1/traces",
		},
		{
			name: "SLOFF generic override appends /v1/traces",
			env:  map[string]string{"SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT": "http://sloff-only:4318"},
			want: "http://sloff-only:4318/v1/traces",
		},
		{
			name: "SLOFF signal-specific override used as-is",
			env: map[string]string{
				"SLOFF_OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://sloff-traces:4318/v1/traces",
			},
			want: "http://sloff-traces:4318/v1/traces",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearOTelEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := resolveOTLPHTTPEndpoint(); got != tc.want {
				t.Fatalf("resolveOTLPHTTPEndpoint() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSetupTracing_DisabledIsZeroCost(t *testing.T) {
	clearOTelEnv(t)
	ctx := context.Background()

	tp, shutdown, err := setupTracing(ctx)
	if err != nil {
		t.Fatalf("setupTracing returned err = %v, want nil when env is empty", err)
	}
	if tp == nil {
		t.Fatal("setupTracing returned nil TracerProvider; expected noop")
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

	tp, shutdown, err := setupTracing(ctx)
	if err != nil {
		t.Fatalf("setupTracing returned err = %v", err)
	}
	if tp == nil {
		t.Fatal("setupTracing returned nil TracerProvider")
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

			_, shutdown, err := setupTracing(ctx)
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

// TestSetupTracing_NeverTouchesGlobalProvider is the load-bearing contract
// for safe in-process embedding: setupTracing must NEVER mutate the otel-go
// global TracerProvider or TextMapPropagator regardless of env state. Without
// this, two concurrent sloff invocations in the same process race on the
// global, and a shutdown from one run would tear down state another run
// depends on (codex review round 6 P2 #2).
func TestSetupTracing_NeverTouchesGlobalProvider(t *testing.T) {
	scenarios := []struct {
		name string
		env  map[string]string
	}{
		{name: "passive disabled (no env)", env: nil},
		{name: "explicit OTEL_SDK_DISABLED=true", env: map[string]string{"OTEL_SDK_DISABLED": "true"}},
		{name: "explicit OTEL_TRACES_EXPORTER=none", env: map[string]string{"OTEL_TRACES_EXPORTER": "none"}},
		{name: "SLOFF_OTEL_SDK_DISABLED=true with shell endpoint", env: map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": "http://host-collector:4318",
			"SLOFF_OTEL_SDK_DISABLED":     "true",
		}},
		{name: "enabled via OTEL_TRACES_EXPORTER=console", env: map[string]string{"OTEL_TRACES_EXPORTER": "console"}},
		{name: "enabled via SLOFF_OTEL_*", env: map[string]string{"SLOFF_OTEL_TRACES_EXPORTER": "console"}},
	}
	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) {
			clearOTelEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			prevProvider := otel.GetTracerProvider()
			prevPropagator := otel.GetTextMapPropagator()
			ctx := context.Background()

			_, shutdown, err := setupTracing(ctx)
			if err != nil {
				t.Fatalf("setupTracing err = %v", err)
			}
			if got := otel.GetTracerProvider(); got != prevProvider {
				t.Fatal("setupTracing mutated the global TracerProvider; in-process embedders / concurrent runs would observe interference")
			}
			if got := otel.GetTextMapPropagator(); got != prevPropagator {
				t.Fatal("setupTracing mutated the global TextMapPropagator; in-process embedders would observe interference")
			}

			if err := shutdown(ctx); err != nil {
				t.Fatalf("shutdown err = %v", err)
			}
			if got := otel.GetTracerProvider(); got != prevProvider {
				t.Fatal("shutdown mutated the global TracerProvider")
			}
		})
	}
}

// TestSetupTracing_DisabledReturnsNoopProvider asserts that the disabled
// path returns a TracerProvider on which Tracer().Start() is recordable as a
// noop (the resulting span has an invalid SpanContext per the OTel spec).
// Without this, runner.New(Options{TracerProvider: tp}) would silently
// produce real spans even when the user asked for silence.
func TestSetupTracing_DisabledReturnsNoopProvider(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	ctx := context.Background()

	tp, shutdown, err := setupTracing(ctx)
	if err != nil {
		t.Fatalf("setupTracing err = %v", err)
	}
	t.Cleanup(func() { _ = shutdown(ctx) })

	_, span := tp.Tracer("test").Start(ctx, "noop-check")
	if span.SpanContext().IsValid() {
		t.Fatal("disabled TracerProvider returned a span with a valid SpanContext; want noop")
	}
	if span.IsRecording() {
		t.Fatal("disabled TracerProvider returned a recording span; want noop")
	}
	span.End()
}

// TestSetupTracing_EnabledReturnsRecordingProvider asserts that the enabled
// path returns a TracerProvider whose tracers actually produce recording
// spans. Together with TestSetupTracing_NeverTouchesGlobalProvider this nails
// down "configured exporter, never via the global".
func TestSetupTracing_EnabledReturnsRecordingProvider(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "console")
	ctx := context.Background()

	tp, shutdown, err := setupTracing(ctx)
	if err != nil {
		t.Fatalf("setupTracing err = %v", err)
	}
	t.Cleanup(func() { _ = shutdown(ctx) })

	_, span := tp.Tracer("test").Start(ctx, "recording-check")
	if !span.SpanContext().IsValid() {
		t.Fatal("enabled TracerProvider returned a span with an invalid SpanContext; want recording")
	}
	if !span.IsRecording() {
		t.Fatal("enabled TracerProvider returned a non-recording span; want recording")
	}
	span.End()
}

// TestSetupTracing_UnsupportedExporterFallsBackToNoop covers the case where a
// shell exports `OTEL_TRACES_EXPORTER` to a value sloff doesn't implement
// (zipkin / jaeger / a comma-separated multi-export list). The OTel SDK spec
// mandates "warn and use the noop tracer" for this, and a hard failure here
// would brick `sloff run` / `graph` for any user whose shell already exports
// trace config for other tools.
func TestSetupTracing_UnsupportedExporterFallsBackToNoop(t *testing.T) {
	cases := []struct {
		name string
		env  string
	}{
		{name: "zipkin (other tool's exporter)", env: "zipkin"},
		{name: "jaeger (other tool's exporter)", env: "jaeger"},
		{name: "comma-separated multi-export", env: "otlp,zipkin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearOTelEnv(t)
			t.Setenv("OTEL_TRACES_EXPORTER", tc.env)
			ctx := context.Background()

			tp, shutdown, err := setupTracing(ctx)
			if err != nil {
				t.Fatalf("setupTracing err = %v, want nil (unsupported exporter must fall back to noop, not fail)", err)
			}
			t.Cleanup(func() { _ = shutdown(ctx) })

			_, span := tp.Tracer("test").Start(ctx, "noop-check")
			if span.IsRecording() {
				t.Errorf("got recording span for OTEL_TRACES_EXPORTER=%q; want noop fallback", tc.env)
			}
			span.End()
		})
	}
}

// TestConsoleSpanWriter_DefaultsToStderr guards the contract that the
// console exporter does not corrupt stdout-driven subcommands such as
// `sloff graph`. If consoleSpanWriter is ever flipped back to os.Stdout
// (or to anything other than os.Stderr), trace JSON would interleave with
// the rendered graph DSL and downstream parsers would break.
func TestConsoleSpanWriter_DefaultsToStderr(t *testing.T) {
	if consoleSpanWriter != os.Stderr {
		t.Fatalf("consoleSpanWriter = %v, want os.Stderr (stdout would corrupt `sloff graph` output)", consoleSpanWriter)
	}
}

// errSentinel is a sanity check that errors.Is is wired correctly when we
// fall through unsupported-exporter handling. Currently unused beyond a
// compile-time reference; kept so future tests can grep for the pattern.
var _ = errors.Is
