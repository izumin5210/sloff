package main

import (
	"context"
	"os"
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

func TestApplySloffPrefixOverrides(t *testing.T) {
	t.Run("SLOFF_ value overrides existing OTEL_ value", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://shell-default:4318")
		t.Setenv("SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT", "http://sloff-only:4318")

		_ = applySloffPrefixOverrides()

		if got := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); got != "http://sloff-only:4318" {
			t.Fatalf("OTEL_EXPORTER_OTLP_ENDPOINT = %q, want SLOFF_ override to win", got)
		}
	})

	t.Run("SLOFF_ value populates unset OTEL_ var", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("SLOFF_OTEL_TRACES_EXPORTER", "console")

		_ = applySloffPrefixOverrides()

		if got := os.Getenv("OTEL_TRACES_EXPORTER"); got != "console" {
			t.Fatalf("OTEL_TRACES_EXPORTER = %q, want %q (populated from SLOFF_)", got, "console")
		}
	})

	t.Run("missing SLOFF_ leaves OTEL_ untouched", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://shell-default:4318")

		_ = applySloffPrefixOverrides()

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

		_ = applySloffPrefixOverrides()

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

		_ = applySloffPrefixOverrides()

		if envOTelEnabled() {
			t.Fatal("envOTelEnabled() = true, want false (SLOFF_ override should silence)")
		}
	})

	t.Run("SLOFF_OTEL_SDK_DISABLED=true silences sloff", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://shell:4318")
		t.Setenv("SLOFF_OTEL_SDK_DISABLED", "true")

		_ = applySloffPrefixOverrides()

		if envOTelEnabled() {
			t.Fatal("envOTelEnabled() = true, want false (SLOFF_OTEL_SDK_DISABLED should win)")
		}
	})

	t.Run("SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT enables sloff when shell is empty", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT", "http://sloff-only:4318")

		_ = applySloffPrefixOverrides()

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

// TestApplySloffPrefixOverrides_Restore covers the contract that the
// applySloffPrefixOverrides return value reverts every key it touched —
// without it, repeated in-process invocations of newRootCmd().Execute would
// leak the SLOFF_-derived OTEL_* values into runs whose env was supposed to
// behave like a fresh shell (codex review P2).
func TestApplySloffPrefixOverrides_Restore(t *testing.T) {
	t.Run("restore reverts overridden value to original", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://shell-default:4318")
		t.Setenv("SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT", "http://sloff-only:4318")

		restore := applySloffPrefixOverrides()
		if got := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); got != "http://sloff-only:4318" {
			t.Fatalf("post-apply OTEL_EXPORTER_OTLP_ENDPOINT = %q, want SLOFF_ override", got)
		}

		restore()
		if got := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); got != "http://shell-default:4318" {
			t.Fatalf("post-restore OTEL_EXPORTER_OTLP_ENDPOINT = %q, want shell value restored", got)
		}
	})

	t.Run("restore unsets keys that were originally absent", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("SLOFF_OTEL_TRACES_EXPORTER", "console")

		restore := applySloffPrefixOverrides()
		if _, ok := os.LookupEnv("OTEL_TRACES_EXPORTER"); !ok {
			t.Fatal("post-apply OTEL_TRACES_EXPORTER unset, want populated from SLOFF_")
		}

		restore()
		if _, ok := os.LookupEnv("OTEL_TRACES_EXPORTER"); ok {
			t.Fatal("post-restore OTEL_TRACES_EXPORTER still set, want unset (was not set originally)")
		}
	})

	t.Run("restore is a no-op when no SLOFF_ overrides apply", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://shell-default:4318")

		restore := applySloffPrefixOverrides()
		restore()

		if got := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); got != "http://shell-default:4318" {
			t.Fatalf("OTEL_EXPORTER_OTLP_ENDPOINT = %q, want shell value untouched", got)
		}
	})
}

// TestSetupTracing_DisabledDoesNotClobberGlobal asserts the disabled path no
// longer overwrites the host process's TracerProvider with a noop. Codex review
// flagged that an in-process host that has configured its own provider would
// silently lose tracing on any sloff invocation that doesn't have OTEL_ env
// set; the fix is to leave the global alone on the disabled path entirely.
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
		t.Fatalf("disabled setupTracing replaced global TracerProvider; want unchanged")
	}
}

// TestSetupTracing_DisabledShutdownRestoresEnv asserts that the shutdown
// returned from a disabled setup still reverts SLOFF_-prefix env mutations,
// keeping subsequent in-process invocations isolated from the previous one.
func TestSetupTracing_DisabledShutdownRestoresEnv(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT", "http://sloff-only:4318")
	// Note: SLOFF_OTEL_TRACES_EXPORTER not set, so envOTelEnabled is still false
	// because the only signal is an endpoint with SLOFF_ prefix... but this gets
	// applied to OTEL_EXPORTER_OTLP_ENDPOINT which then enables. So we also need
	// to flip envOTelEnabled off via SLOFF_OTEL_TRACES_EXPORTER=none to stay on
	// the disabled path while still exercising the env mutation.
	t.Setenv("SLOFF_OTEL_TRACES_EXPORTER", "none")
	ctx := context.Background()

	shutdown, err := setupTracing(ctx)
	if err != nil {
		t.Fatalf("setupTracing returned err = %v", err)
	}
	if got := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); got != "http://sloff-only:4318" {
		t.Fatalf("during run OTEL_EXPORTER_OTLP_ENDPOINT = %q, want SLOFF_ override applied", got)
	}

	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown returned err = %v", err)
	}
	if _, ok := os.LookupEnv("OTEL_EXPORTER_OTLP_ENDPOINT"); ok {
		t.Fatal("post-shutdown OTEL_EXPORTER_OTLP_ENDPOINT still set, want SLOFF_ override reverted")
	}
}

// TestSetupTracing_EnabledRestoresProviderAndEnv covers the enabled-path
// mirror: sloff installs its own TracerProvider and propagator, but the
// shutdown must put the prior provider and env back so the host process is
// not stuck with a sloff-shut-down provider after the command returns.
func TestSetupTracing_EnabledRestoresProviderAndEnv(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "console")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://shell-default:4318")
	t.Setenv("SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT", "http://sloff-only:4318")

	prevProvider := otel.GetTracerProvider()
	ctx := context.Background()

	shutdown, err := setupTracing(ctx)
	if err != nil {
		t.Fatalf("setupTracing returned err = %v", err)
	}
	if got := otel.GetTracerProvider(); got == prevProvider {
		t.Fatal("enabled setupTracing did not install a new TracerProvider; want sloff's TP active")
	}
	if got := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); got != "http://sloff-only:4318" {
		t.Fatalf("during run OTEL_EXPORTER_OTLP_ENDPOINT = %q, want SLOFF_ override applied", got)
	}

	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown returned err = %v", err)
	}
	if got := otel.GetTracerProvider(); got != prevProvider {
		t.Fatal("post-shutdown TracerProvider not restored to caller's prior value")
	}
	if got := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); got != "http://shell-default:4318" {
		t.Fatalf("post-shutdown OTEL_EXPORTER_OTLP_ENDPOINT = %q, want shell-default restored", got)
	}
}
