package timing

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

var base = time.Unix(1_000_000, 0)

func sid(b byte) trace.SpanID {
	if b == 0 {
		return trace.SpanID{} // invalid (root's parent)
	}
	return trace.SpanID{b}
}

// rec builds a synthetic finished-span projection at second offsets from base.
func rec(name string, id, parent byte, startSec, endSec float64) record {
	return record{
		name:     name,
		spanID:   sid(id),
		parentID: sid(parent),
		start:    base.Add(time.Duration(startSec * float64(time.Second))),
		end:      base.Add(time.Duration(endSec * float64(time.Second))),
	}
}

func TestSummarize_Empty(t *testing.T) {
	if got := Summarize(nil); got != "" {
		t.Fatalf("Summarize(nil) = %q, want empty", got)
	}
}

func TestSummarize_NoRoot(t *testing.T) {
	// Two spans that both reference each other's presence but neither is a
	// recognisable root and both have in-set parents → still resolves a root via
	// the earliest-orphan rule. Here both parents are in-set, so findRoot fails
	// and Summarize returns "" rather than a misleading report.
	recs := []record{
		{name: "a", spanID: sid(1), parentID: sid(2), start: base, end: base.Add(time.Second)},
		{name: "b", spanID: sid(2), parentID: sid(1), start: base, end: base.Add(time.Second)},
	}
	if got := Summarize(recs); got != "" {
		t.Fatalf("Summarize with no root = %q, want empty", got)
	}
}

func TestSummarize_PhasesAndTail(t *testing.T) {
	root := rec(spanRoot, 1, 0, 0, 30)
	resolve := rec("runner.resolve", 2, 1, 1, 4)
	prefetch := rec(spanPrefetch, 3, 1, 4, 6.4)
	prefetchLoad := rec(spanPrefetchLoad, 4, 3, 6.0, 6.4) // 0.4s load → keys 2.0s
	tasksRun := rec(spanTasksRun, 5, 1, 6.4, 26)

	genRun := rec(spanTaskRun, 6, 5, 20, 26) // last to finish
	genRun.taskLabel = "proto:generate"
	genRun.hasHit, genRun.hit = true, false // RUN
	genExec := rec(spanTaskExec, 7, 6, 20, 25.9)
	genHash := rec(spanTaskHashInput, 8, 6, 20, 20.1)

	pothosRun := rec(spanTaskRun, 9, 5, 6.4, 7)
	pothosRun.taskLabel = "proto:buf-pothos-hr"
	pothosRun.hasHit, pothosRun.hit = true, true // SKIP
	pothosHash := rec(spanTaskHashInput, 10, 9, 6.4, 6.45)

	resolverSpan := rec("resolver.go-local[protoc-gen-pothos]", 11, 2, 1, 3)
	resolverSpan.toolName = "protoc-gen-pothos"

	out := Summarize([]record{
		root, resolve, prefetch, prefetchLoad, tasksRun,
		genRun, genExec, genHash, pothosRun, pothosHash, resolverSpan,
	})

	assertContains(t, out, "total wall: 30.00s")
	assertContains(t, out, "runner.resolve")
	assertContains(t, out, "runner.fingerprint.prefetch")
	// prefetch phase split into optimistic-key compute vs LoadMany.
	assertContains(t, out, "keys 2.00s + load 400ms")
	// resolver breakdown surfaces the tool.
	assertContains(t, out, "protoc-gen-pothos")
	// task tail lists the latest-finishing task first, with RUN, and generate
	// must appear above the earlier pothos SKIP.
	assertContains(t, out, "proto:generate")
	assertContains(t, out, "RUN")
	assertContains(t, out, "SKIP")
	// Within the task tail, the latest-finishing task (generate, ends at 26s)
	// must precede the earlier pothos SKIP (ends at 7s).
	tail := out[strings.Index(out, "task tail"):]
	gi := strings.Index(tail, "proto:generate")
	pi := strings.Index(tail, "proto:buf-pothos-hr")
	if gi < 0 || pi < 0 || gi > pi {
		t.Fatalf("expected generate before pothos in task tail (gi=%d pi=%d)\n%s", gi, pi, tail)
	}
	// per-task aggregate rows.
	assertContains(t, out, "task.run")
	assertContains(t, out, "task.hash_inputs")
}

func TestSummarize_RootFallbackToOrphan(t *testing.T) {
	// No sloff.run span, but one span has a parent that isn't captured → it is
	// the top-level anchor.
	orphan := rec("some.top", 1, 99, 0, 10) // parent 99 not in set
	child := rec("child", 2, 1, 1, 2)
	out := Summarize([]record{orphan, child})
	assertContains(t, out, "total wall: 10.00s")
	assertContains(t, out, "some.top")
}

// TestCollector_LiveSpanTree drives a real SDK TracerProvider with the Collector
// attached, emitting a span tree shaped like a real run, and asserts the summary
// rendered on Shutdown reflects the structure. This exercises OnEnd's attribute
// extraction and the SpanProcessor wiring, not just the pure formatter.
func TestCollector_LiveSpanTree(t *testing.T) {
	var buf bytes.Buffer
	col := NewCollector(&buf)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(col))
	tr := tp.Tracer("test")

	ctx, root := tr.Start(context.Background(), spanRoot)

	_, resolve := tr.Start(ctx, "resolver.go-local[t]", trace.WithAttributes(
		attribute.String("sloff.tool.name", "t"),
	))
	resolve.End()

	pctx, prefetch := tr.Start(ctx, spanPrefetch)
	_, pload := tr.Start(pctx, spanPrefetchLoad)
	pload.End()
	prefetch.End()

	rctx, tasksRun := tr.Start(ctx, spanTasksRun)
	tctx, taskRun := tr.Start(rctx, spanTaskRun, trace.WithAttributes(
		attribute.String("sloff.spec", "proto"),
		attribute.String("sloff.task.name", "generate"),
		attribute.Bool("sloff.fingerprint.hit", false),
	))
	_, hash := tr.Start(tctx, spanTaskHashInput)
	hash.End()
	taskRun.End()
	tasksRun.End()

	root.End()
	if err := tp.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	out := buf.String()
	assertContains(t, out, "=== sloff timing summary")
	assertContains(t, out, "proto:generate")
	assertContains(t, out, "RUN")
	assertContains(t, out, "t") // resolver tool name

	// Shutdown is idempotent: a second call prints nothing more.
	before := buf.Len()
	_ = col.Shutdown(context.Background())
	if buf.Len() != before {
		t.Fatalf("second Shutdown wrote again; want idempotent")
	}
}

func TestFmtDur(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{2500 * time.Millisecond, "2.50s"},
		{1200 * time.Millisecond, "1.20s"},
		{400 * time.Millisecond, "400ms"},
		{999 * time.Microsecond, "999µs"},
		{-5 * time.Second, "0µs"},
	}
	for _, tc := range cases {
		if got := fmtDur(tc.d); got != tc.want {
			t.Errorf("fmtDur(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("output missing %q\n--- output ---\n%s", needle, haystack)
	}
}
