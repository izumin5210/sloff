package main

import (
	"context"
	"os"
	"testing"
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

func TestApplySloffPrefixOverrides(t *testing.T) {
	t.Run("SLOFF_ value overrides existing OTEL_ value", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://shell-default:4318")
		t.Setenv("SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT", "http://sloff-only:4318")

		applySloffPrefixOverrides()

		if got := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); got != "http://sloff-only:4318" {
			t.Fatalf("OTEL_EXPORTER_OTLP_ENDPOINT = %q, want SLOFF_ override to win", got)
		}
	})

	t.Run("SLOFF_ value populates unset OTEL_ var", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("SLOFF_OTEL_TRACES_EXPORTER", "console")

		applySloffPrefixOverrides()

		if got := os.Getenv("OTEL_TRACES_EXPORTER"); got != "console" {
			t.Fatalf("OTEL_TRACES_EXPORTER = %q, want %q (populated from SLOFF_)", got, "console")
		}
	})

	t.Run("missing SLOFF_ leaves OTEL_ untouched", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://shell-default:4318")

		applySloffPrefixOverrides()

		if got := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); got != "http://shell-default:4318" {
			t.Fatalf("OTEL_EXPORTER_OTLP_ENDPOINT = %q, want shell value preserved", got)
		}
	})

	t.Run("empty SLOFF_ value still overrides (explicit blanking)", func(t *testing.T) {
		// Setting SLOFF_OTEL_TRACES_EXPORTER="" is a deliberate way to tell sloff
		// "ignore the shell-wide OTEL_TRACES_EXPORTER". envOTelEnabled then sees an
		// empty string and short-circuits as if no exporter env is set.
		clearOTelEnv(t)
		t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
		t.Setenv("SLOFF_OTEL_TRACES_EXPORTER", "")

		applySloffPrefixOverrides()

		if got, ok := os.LookupEnv("OTEL_TRACES_EXPORTER"); !ok || got != "" {
			t.Fatalf("OTEL_TRACES_EXPORTER = (%q, %v), want (\"\", true)", got, ok)
		}
	})
}

func TestEnvOTelEnabled(t *testing.T) {
	cases := []struct {
		name string
		set  map[string]string
		want bool
	}{
		{
			name: "all unset",
			set:  nil,
			want: false,
		},
		{
			name: "OTLP endpoint set",
			set:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318"},
			want: true,
		},
		{
			name: "OTLP traces endpoint set",
			set:  map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://localhost:4318/v1/traces"},
			want: true,
		},
		{
			name: "traces exporter set to otlp",
			set:  map[string]string{"OTEL_TRACES_EXPORTER": "otlp"},
			want: true,
		},
		{
			name: "traces exporter explicitly none",
			set:  map[string]string{"OTEL_TRACES_EXPORTER": "none"},
			want: false,
		},
		{
			name: "traces exporter none case-insensitive",
			set:  map[string]string{"OTEL_TRACES_EXPORTER": "NONE"},
			want: false,
		},
		{
			name: "endpoint set but SDK disabled wins",
			set: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318",
				"OTEL_SDK_DISABLED":           "true",
			},
			want: false,
		},
		{
			name: "SDK disabled case-insensitive",
			set: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318",
				"OTEL_SDK_DISABLED":           "TRUE",
			},
			want: false,
		},
		{
			name: "SDK disabled false does not block enable",
			set: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318",
				"OTEL_SDK_DISABLED":           "false",
			},
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

func TestEnvOTelEnabled_SloffPrefixIntegration(t *testing.T) {
	t.Run("SLOFF_OTEL_TRACES_EXPORTER=none silences sloff while shell is otlp", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://shell:4318")
		t.Setenv("SLOFF_OTEL_TRACES_EXPORTER", "none")

		applySloffPrefixOverrides()

		if envOTelEnabled() {
			t.Fatal("envOTelEnabled() = true, want false (SLOFF_ override should silence)")
		}
	})

	t.Run("SLOFF_OTEL_SDK_DISABLED=true silences sloff", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://shell:4318")
		t.Setenv("SLOFF_OTEL_SDK_DISABLED", "true")

		applySloffPrefixOverrides()

		if envOTelEnabled() {
			t.Fatal("envOTelEnabled() = true, want false (SLOFF_OTEL_SDK_DISABLED should win)")
		}
	})

	t.Run("SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT enables sloff when shell is empty", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT", "http://sloff-only:4318")

		applySloffPrefixOverrides()

		if !envOTelEnabled() {
			t.Fatal("envOTelEnabled() = false, want true (SLOFF_ endpoint should enable)")
		}
		if got := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); got != "http://sloff-only:4318" {
			t.Fatalf("OTEL_EXPORTER_OTLP_ENDPOINT = %q, want SLOFF_ value populated", got)
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
	// console exporter writes to stdout; safe in tests and avoids any network I/O.
	t.Setenv("OTEL_TRACES_EXPORTER", "console")
	ctx := context.Background()

	shutdown, err := setupTracing(ctx)
	if err != nil {
		t.Fatalf("setupTracing returned err = %v", err)
	}
	if shutdown == nil {
		t.Fatal("setupTracing returned nil shutdown")
	}
	// Shutdown must not error for the canonical happy path; console exporter has
	// no remote connection to drain. A live context (rather than a canceled one)
	// is required because BatchSpanProcessor shutdown participates in ctx.Done.
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown returned err = %v, want nil for console exporter", err)
	}
}
