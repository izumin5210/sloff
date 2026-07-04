// Package timing turns the OpenTelemetry spans the runner already emits into a
// human-readable, run-end phase/task breakdown printed to stderr.
//
// It exists so a developer profiling `sloff run` can see where wall-clock went
// (resolve vs prefetch vs the task schedule vs generate) without standing up an
// OTLP collector. The Collector is an sdktrace.SpanProcessor: cmd/sloff attaches
// it to the sloff-local TracerProvider only when SLOFF_DEBUG_TIMING is set, so
// the default path pays nothing (the runner keeps its noop TracerProvider and
// never records a span). Because every phase already carries a span (ADR-0018),
// this adds observation, not instrumentation — the numbers are exactly the spans
// the OTLP exporter would ship.
package timing

import (
	"context"
	"io"
	"sync"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Span names the Collector reads by identity. Kept as constants so a rename in
// the runner surfaces here as a compile break rather than a silently empty row.
const (
	spanRoot          = "sloff.run"
	spanRootGraph     = "sloff.graph"
	spanTasksRun      = "runner.tasks.run"
	spanTaskRun       = "runner.task.run"
	spanTaskExec      = "runner.task.exec"
	spanTaskHashInput = "runner.task.hash_inputs"
	spanFingerLoad    = "runner.fingerprint.load"
	spanPrefetch      = "runner.fingerprint.prefetch"
	spanPrefetchLoad  = "runner.fingerprint.prefetch.load"
)

// record is the minimal projection of a finished span the summary needs. Storing
// a projection (rather than the ReadOnlySpan) bounds memory on large runs and
// makes the render logic independent of the SDK span type.
type record struct {
	name      string
	spanID    trace.SpanID
	parentID  trace.SpanID
	start     time.Time
	end       time.Time
	taskLabel string // sloff.spec + ":" + sloff.task.name, when present
	toolName  string // sloff.tool.name, when present
	hasHit    bool   // whether sloff.fingerprint.hit was set (i.e. this is a task.run)
	hit       bool   // value of sloff.fingerprint.hit
}

func (r record) dur() time.Duration { return r.end.Sub(r.start) }

// Collector accumulates finished spans and renders the summary on Shutdown.
// Safe for concurrent OnEnd (task spans end from many goroutines at once).
type Collector struct {
	w io.Writer

	mu      sync.Mutex
	records []record
	printed bool
}

// NewCollector returns a Collector that writes its summary to w (os.Stderr in
// the CLI).
func NewCollector(w io.Writer) *Collector { return &Collector{w: w} }

var _ sdktrace.SpanProcessor = (*Collector)(nil)

// OnStart is required by the SpanProcessor interface; timing only needs ended
// spans (start+end are read from the ReadOnlySpan in OnEnd).
func (c *Collector) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

// OnEnd projects the finished span into a record. It never blocks on I/O; the
// summary is rendered once at Shutdown.
func (c *Collector) OnEnd(s sdktrace.ReadOnlySpan) {
	rec := record{
		name:     s.Name(),
		spanID:   s.SpanContext().SpanID(),
		parentID: s.Parent().SpanID(),
		start:    s.StartTime(),
		end:      s.EndTime(),
	}
	var specName, taskName string
	for _, kv := range s.Attributes() {
		switch kv.Key {
		case "sloff.spec":
			specName = kv.Value.AsString()
		case "sloff.task.name":
			taskName = kv.Value.AsString()
		case "sloff.tool.name":
			rec.toolName = kv.Value.AsString()
		case "sloff.fingerprint.hit":
			rec.hasHit = true
			rec.hit = kv.Value.AsBool()
		}
	}
	if taskName != "" {
		rec.taskLabel = taskLabel(specName, taskName)
	}
	c.mu.Lock()
	c.records = append(c.records, rec)
	c.mu.Unlock()
}

// ForceFlush is a no-op: the Collector holds nothing to flush to a backend.
func (c *Collector) ForceFlush(context.Context) error { return nil }

// Shutdown renders the summary exactly once. Idempotent so a double Shutdown
// (TracerProvider.Shutdown plus a defensive caller) prints one report.
func (c *Collector) Shutdown(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.printed {
		return nil
	}
	c.printed = true
	c.render()
	return nil
}

func taskLabel(spec, name string) string {
	if spec == "" || spec == "." {
		return name
	}
	return spec + ":" + name
}
