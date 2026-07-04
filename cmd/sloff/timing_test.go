package main

import (
	"context"
	"reflect"
	"testing"
)

func TestDebugTimingEnabled(t *testing.T) {
	cases := []struct {
		name    string
		set     bool
		val     string
		want    bool
		wantErr bool
	}{
		{name: "unset", set: false, want: false},
		{name: "empty behaves like unset", set: true, val: "", want: false},
		{name: "1 enables", set: true, val: "1", want: true},
		{name: "true enables", set: true, val: "true", want: true},
		{name: "0 disables", set: true, val: "0", want: false},
		{name: "false disables", set: true, val: "false", want: false},
		{name: "garbage fails loudly", set: true, val: "yes-please", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearOTelEnv(t)
			if tc.set {
				t.Setenv(debugTimingEnv, tc.val)
			}
			got, err := debugTimingEnabled()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tc.val)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("debugTimingEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSetupTracing_TimingEnablesRecordingWithoutExporter is the core Phase-P
// contract: SLOFF_DEBUG_TIMING alone must produce a recording TracerProvider
// even with no OTLP/console export configured, so the runner's spans are
// captured for the summary.
func TestSetupTracing_TimingEnablesRecordingWithoutExporter(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv(debugTimingEnv, "1")
	ctx := context.Background()

	tp, shutdown, err := setupTracing(ctx)
	if err != nil {
		t.Fatalf("setupTracing err = %v", err)
	}
	t.Cleanup(func() { _ = shutdown(ctx) })

	_, span := tp.Tracer("test").Start(ctx, "recording-check")
	if !span.IsRecording() {
		t.Fatal("SLOFF_DEBUG_TIMING=1 produced a non-recording span; timing summary would be empty")
	}
	span.End()
}

// TestSetupTracing_TimingDisabledStaysNoop guards that a disabled/garbage-free
// timing gate does not accidentally light up the SDK path.
func TestSetupTracing_TimingDisabledStaysNoop(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv(debugTimingEnv, "0")
	ctx := context.Background()

	tp, shutdown, err := setupTracing(ctx)
	if err != nil {
		t.Fatalf("setupTracing err = %v", err)
	}
	t.Cleanup(func() { _ = shutdown(ctx) })

	_, span := tp.Tracer("test").Start(ctx, "noop-check")
	if span.IsRecording() {
		t.Fatal("SLOFF_DEBUG_TIMING=0 produced a recording span; want noop")
	}
	span.End()
}

// TestSetupTracing_TimingErrorPropagates ensures a malformed SLOFF_DEBUG_TIMING
// fails setup rather than silently disabling the summary.
func TestSetupTracing_TimingErrorPropagates(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv(debugTimingEnv, "not-a-bool")
	if _, _, err := setupTracing(context.Background()); err == nil {
		t.Fatal("setupTracing accepted a malformed SLOFF_DEBUG_TIMING; want error")
	}
}

// TestSetupTracing_TimingNeverMutatesEnv extends the env-immutability contract
// to the timing gate: enabling the summary must not write any OTEL_* keys.
func TestSetupTracing_TimingNeverMutatesEnv(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv(debugTimingEnv, "1")
	before := snapshotOTELEnv()
	ctx := context.Background()

	_, shutdown, err := setupTracing(ctx)
	if err != nil {
		t.Fatalf("setupTracing err = %v", err)
	}
	t.Cleanup(func() { _ = shutdown(ctx) })

	if after := snapshotOTELEnv(); !reflect.DeepEqual(before, after) {
		t.Fatalf("timing setup mutated OTEL_* env\n before=%v\n after=%v", before, after)
	}
}
