package runner_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint/local"
	"github.com/izumin5210/sloff/internal/sloff/preflight"
	preflightpnpm "github.com/izumin5210/sloff/internal/sloff/preflight/pnpmlocal"
	"github.com/izumin5210/sloff/internal/sloff/runner"
	"github.com/izumin5210/sloff/internal/sloff/spec"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/golocal"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/lister"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/pnpmlocal"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/script"
)

// runOnceForTrace runs the runner once with a fresh in-memory SpanRecorder
// wired through Options.TracerProvider, and returns the recorder for caller
// inspection. Each invocation uses its own provider; sloff never touches the
// otel-go global TracerProvider, so concurrent / sequential calls don't share
// state and the test process's host instrumentation is untouched.
func runOnceForTrace(t *testing.T, h *harness) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	specs, err := spec.Discover(h.workdir, "**/sloff.yml")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	resolverReg := toolresolver.NewRegistry()
	resolverReg.Register(script.New(h.workdir))
	resolverReg.Register(golocal.New(h.workdir, lister.NewMemoized(lister.NewGoPackages(h.workdir))))
	pnpmRes, err := pnpmlocal.New(h.workdir, pnpmlocal.GitLsFiles)
	if err != nil {
		t.Fatalf("pnpmlocal.New: %v", err)
	}
	resolverReg.Register(pnpmRes)
	preflightReg := preflight.NewRegistry()
	preflightReg.Register(preflightpnpm.New(h.workdir))
	r := runner.New(runner.Options{
		RepoRoot:       h.workdir,
		Specs:          specs,
		Storage:        local.New(h.workdir, local.WithClock(func() time.Time { return fixedClock })),
		Resolvers:      resolverReg,
		Preflight:      preflightReg,
		TracerProvider: tp,
	})
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rec
}

// findSpan returns the first ended span with the given name, or nil if absent.
// Span ordering is not deterministic (multiple goroutines may end concurrently),
// so tests should not rely on slice indices.
func findSpan(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// findSpansByName returns every span with the given name (per-task / per-tool
// spans share names, distinguished by attributes).
func findSpansByName(spans []sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

// childrenOf returns the spans whose parent is parent.SpanContext(). This walks
// the recorder's flat ended-span list and filters by ParentSpanID — span tree
// reconstruction is the test's responsibility because the SDK records spans in
// end-order, not tree-order.
func childrenOf(spans []sdktrace.ReadOnlySpan, parent sdktrace.ReadOnlySpan) []sdktrace.ReadOnlySpan {
	pid := parent.SpanContext().SpanID()
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Parent().SpanID() == pid {
			out = append(out, s)
		}
	}
	return out
}

// attrString returns the string-valued attribute with the given key on s, or
// "" if absent. Tests use this for sloff.fingerprint.state and similar string attrs.
func attrString(s sdktrace.ReadOnlySpan, key string) string {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsString()
		}
	}
	return ""
}

// attrBool returns the bool-valued attribute with the given key on s, or false
// (with a `found` flag) if absent. Used to assert sloff.fingerprint.hit.
func attrBool(s sdktrace.ReadOnlySpan, key string) (bool, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsBool(), true
		}
	}
	return false, false
}

// dumpSpans renders the span list as parent → child relationships for fixture
// debugging. Only printed when an assertion fails.
func dumpSpans(spans []sdktrace.ReadOnlySpan) string {
	var out strings.Builder
	for _, s := range spans {
		out.WriteString(fmt.Sprintf("  %s parent=%s\n", s.Name(), s.Parent().SpanID()))
	}
	return out.String()
}

func TestTrace_FirstRunCacheMiss(t *testing.T) {
	h := setupHarness(t, "first-run-writes-record")
	rec := runOnceForTrace(t, h)

	spans := rec.Ended()

	// Each phase span must exist exactly once at the top level (cmd/sloff is not
	// in scope for this test, so phases have no parent under the recorder).
	for _, name := range []string{
		"runner.preflight",
		"runner.resolve.inputs",
		"runner.resolve.versions",
		"runner.collect_tasks",
		"runner.depgraph.build",
		"runner.tasks.run",
	} {
		if findSpan(spans, name) == nil {
			t.Errorf("missing phase span %q\nspans:\n%s", name, dumpSpans(spans))
		}
	}

	// Per-tool resolver spans should land under resolve.inputs and resolve.versions.
	inputsSpan := findSpan(spans, "runner.resolve.inputs")
	if inputsSpan == nil {
		t.Fatal("runner.resolve.inputs missing — cannot continue")
	}
	resolverChildren := childrenOf(spans, inputsSpan)
	wantChild := "resolver.script[versioner]"
	found := false
	for _, c := range resolverChildren {
		if c.Name() == wantChild {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected child %q under runner.resolve.inputs, got %d children", wantChild, len(resolverChildren))
	}

	// runner.task.run must exist with fingerprint.hit=false on first run, and have all
	// three I/O sub-spans (load / exec / save).
	taskSpans := findSpansByName(spans, "runner.task.run")
	if len(taskSpans) != 1 {
		t.Fatalf("runner.task.run count = %d, want 1\nspans:\n%s", len(taskSpans), dumpSpans(spans))
	}
	taskSpan := taskSpans[0]

	if hit, ok := attrBool(taskSpan, "sloff.fingerprint.hit"); !ok || hit {
		t.Errorf("sloff.fingerprint.hit on first run = (%v, found=%v), want (false, true)", hit, ok)
	}

	taskChildren := childrenOf(spans, taskSpan)
	gotChildNames := map[string]int{}
	for _, c := range taskChildren {
		gotChildNames[c.Name()]++
	}
	for _, want := range []string{"runner.fingerprint.load", "runner.task.exec", "runner.fingerprint.queue"} {
		if gotChildNames[want] != 1 {
			t.Errorf("child %q under runner.task.run: got %d, want 1\nchildren: %v", want, gotChildNames[want], gotChildNames)
		}
	}

	// fingerprint.load on a cold run should report the not_found state.
	loadSpan := findSpan(taskChildren, "runner.fingerprint.load")
	if loadSpan != nil {
		if state := attrString(loadSpan, "sloff.fingerprint.state"); state != "not_found" {
			t.Errorf("runner.fingerprint.load sloff.fingerprint.state = %q, want \"not_found\"", state)
		}
	}
}

func TestTrace_SecondRunCacheHit(t *testing.T) {
	h := setupHarness(t, "first-run-writes-record")

	// Prime the cache (recorder discarded — we only inspect the second run).
	_ = runOnceForTrace(t, h)

	// Second run hits the cache and must skip exec + save.
	rec := runOnceForTrace(t, h)
	spans := rec.Ended()

	taskSpans := findSpansByName(spans, "runner.task.run")
	if len(taskSpans) != 1 {
		t.Fatalf("runner.task.run count = %d, want 1\nspans:\n%s", len(taskSpans), dumpSpans(spans))
	}
	taskSpan := taskSpans[0]

	if hit, ok := attrBool(taskSpan, "sloff.fingerprint.hit"); !ok || !hit {
		t.Errorf("sloff.fingerprint.hit on second run = (%v, found=%v), want (true, true)", hit, ok)
	}

	taskChildren := childrenOf(spans, taskSpan)
	for _, c := range taskChildren {
		switch c.Name() {
		case "runner.task.exec":
			t.Errorf("runner.task.exec should not run on cache hit; got span")
		case "runner.fingerprint.queue":
			t.Errorf("runner.fingerprint.queue should not run on cache hit; got span")
		}
	}

	loadSpan := findSpan(taskChildren, "runner.fingerprint.load")
	if loadSpan == nil {
		t.Fatal("runner.fingerprint.load missing on second run")
	}
	if state := attrString(loadSpan, "sloff.fingerprint.state"); state != "hit" {
		t.Errorf("runner.fingerprint.load sloff.fingerprint.state = %q, want \"hit\"", state)
	}
}

func TestTrace_TaskSpanCarriesIdentity(t *testing.T) {
	h := setupHarness(t, "first-run-writes-record")
	rec := runOnceForTrace(t, h)

	taskSpan := findSpan(rec.Ended(), "runner.task.run")
	if taskSpan == nil {
		t.Fatal("runner.task.run missing")
	}

	want := map[attribute.Key]string{
		"sloff.spec":      "spec",
		"sloff.task.name": "copy",
	}
	for key, expected := range want {
		got := attrString(taskSpan, string(key))
		if got != expected {
			t.Errorf("task span attribute %s = %q, want %q", key, got, expected)
		}
	}
}
