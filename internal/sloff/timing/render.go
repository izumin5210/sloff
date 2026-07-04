package timing

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// tailCount bounds the per-task tables so the summary stays readable on a
// several-hundred-task run; the tail is where a scheduling problem shows.
const tailCount = 12

// render writes the summary to the Collector's writer. Called once under the
// Collector mutex from Shutdown.
func (c *Collector) render() {
	out := Summarize(c.records)
	if out == "" {
		return
	}
	_, _ = c.w.Write([]byte(out))
}

// Summarize renders the phase/task breakdown for a set of finished spans. It is
// the pure core of render: same records in, same string out, no I/O — so tests
// can assert on the report without a TracerProvider or a live run.
func Summarize(records []record) string {
	if len(records) == 0 {
		return ""
	}

	byID := make(map[trace.SpanID]record, len(records))
	childrenByParent := map[trace.SpanID][]record{}
	for _, r := range records {
		byID[r.spanID] = r
		childrenByParent[r.parentID] = append(childrenByParent[r.parentID], r)
	}

	root, ok := findRoot(records)
	if !ok {
		return ""
	}
	origin := root.start

	var b strings.Builder
	b.WriteString("\n=== sloff timing summary (SLOFF_DEBUG_TIMING) ===\n")
	fmt.Fprintf(&b, "total wall: %s  (%s)\n", fmtDur(root.dur()), root.name)

	// Phases: the direct children of the root span, in start order. Each already
	// wraps one runner phase (ADR-0018), so their durations are the phase walls.
	phases := append([]record(nil), childrenByParent[root.spanID]...)
	sort.Slice(phases, func(i, j int) bool { return phases[i].start.Before(phases[j].start) })
	if len(phases) > 0 {
		b.WriteString("phases:\n")
		for _, p := range phases {
			extra := ""
			if p.name == spanPrefetch {
				// Split the one prefetch phase into optimistic-key compute vs the
				// single LoadMany round-trip so a remote-backend RTT is separable
				// from local hashing (the B3 decision hinges on which dominates).
				if load := childDur(childrenByParent[p.spanID], spanPrefetchLoad); load > 0 {
					extra = fmt.Sprintf("   (keys %s + load %s)", fmtDur(p.dur()-load), fmtDur(load))
				}
			}
			fmt.Fprintf(&b, "  %-34s %9s%s\n", p.name, fmtDur(p.dur()), extra)
		}
	}

	// Resolver cost, per tool: resolveContribs fans out one span per referenced
	// tool. Surfacing the slowest tools tells B3 whether tool resolution (go
	// packages.Load / pnpm lockfile walk) is worth a persistent cache.
	if res := resolverRows(records); len(res) > 0 {
		b.WriteString("resolve (per tool, slowest first):\n")
		for _, r := range res {
			fmt.Fprintf(&b, "  %-40s %9s\n", truncate(r.toolName, 40), fmtDur(r.dur()))
		}
	}

	// Per-task aggregates. hash_inputs is the exact cost the B3-1 "skip re-verify"
	// optimisation removes; exec is generator wall; fingerprint.load is the
	// lookup + output re-hash.
	b.WriteString("per-task (count / sum / max):\n")
	for _, name := range []string{spanTaskRun, spanTaskHashInput, spanFingerLoad, spanTaskExec} {
		a := aggregate(records, byID, name)
		if a.count == 0 {
			continue
		}
		fmt.Fprintf(&b, "  %-26s %4d / %9s / %9s  %s\n",
			shortName(name), a.count, fmtDur(a.sum), fmtDur(a.max), a.maxLabel)
	}

	// Task tail: the last tasks to finish, which is where a bad schedule (a deep
	// chain starved behind a wide wave) shows up as late-starting critical work.
	tasks := filterByName(records, spanTaskRun)
	if len(tasks) > 0 {
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].end.After(tasks[j].end) })
		n := min(tailCount, len(tasks))
		fmt.Fprintf(&b, "task tail (last %d to finish, [start..end] rel to run start):\n", n)
		for _, t := range tasks[:n] {
			fmt.Fprintf(&b, "  %-40s [%7s .. %7s] %8s  %s\n",
				truncate(t.taskLabel, 40),
				fmtDur(t.start.Sub(origin)), fmtDur(t.end.Sub(origin)),
				fmtDur(t.dur()), runOrSkip(t))
		}
	}
	return b.String()
}

// findRoot returns the run's root span: the sloff.run / sloff.graph span, or
// failing that any span with no in-set parent (the earliest such). Returns
// ok=false only when records is empty of a usable anchor.
func findRoot(records []record) (record, bool) {
	ids := make(map[trace.SpanID]struct{}, len(records))
	for _, r := range records {
		ids[r.spanID] = struct{}{}
	}
	var best record
	var found bool
	for _, r := range records {
		if r.name == spanRoot || r.name == spanRootGraph {
			return r, true
		}
		// A span whose parent is not among the captured spans is a top-level
		// anchor (the run root, or a detached flush span). Keep the earliest.
		if _, hasParent := ids[r.parentID]; !hasParent {
			if !found || r.start.Before(best.start) {
				best, found = r, true
			}
		}
	}
	return best, found
}

// resolverRow is one tool's resolution span for the per-tool table.
type resolverRow = record

func resolverRows(records []record) []resolverRow {
	var out []resolverRow
	for _, r := range records {
		if strings.HasPrefix(r.name, "resolver.") && r.toolName != "" {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].dur() > out[j].dur() })
	if len(out) > tailCount {
		out = out[:tailCount]
	}
	return out
}

type aggregation struct {
	count    int
	sum      time.Duration
	max      time.Duration
	maxLabel string
}

// aggregate folds every span named name into a count/sum/max, attributing the
// max to a task label (via the span itself or its parent task.run span).
func aggregate(records []record, byID map[trace.SpanID]record, name string) aggregation {
	var a aggregation
	for _, r := range records {
		if r.name != name {
			continue
		}
		a.count++
		d := r.dur()
		a.sum += d
		if d >= a.max {
			a.max = d
			a.maxLabel = labelOf(r, byID)
		}
	}
	return a
}

// labelOf resolves a task label for a span: its own, else its parent task.run's.
func labelOf(r record, byID map[trace.SpanID]record) string {
	if r.taskLabel != "" {
		return r.taskLabel
	}
	if p, ok := byID[r.parentID]; ok && p.taskLabel != "" {
		return p.taskLabel
	}
	return ""
}

func filterByName(records []record, name string) []record {
	var out []record
	for _, r := range records {
		if r.name == name {
			out = append(out, r)
		}
	}
	return out
}

// childDur returns the duration of the first child span named name (0 if none).
func childDur(children []record, name string) time.Duration {
	for _, c := range children {
		if c.name == name {
			return c.dur()
		}
	}
	return 0
}

func runOrSkip(r record) string {
	if !r.hasHit {
		return ""
	}
	if r.hit {
		return "SKIP"
	}
	return "RUN"
}

// shortName strips the common "runner." prefix so the per-task table lines up.
func shortName(name string) string { return strings.TrimPrefix(name, "runner.") }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// fmtDur renders a duration at a fixed, readable precision: seconds with two
// decimals above 1s, whole milliseconds below, microseconds below 1ms.
func fmtDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= time.Second:
		return fmt.Sprintf("%.2fs", d.Seconds())
	case d >= time.Millisecond:
		return fmt.Sprintf("%dms", d.Milliseconds())
	default:
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
}
