package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// cmdTracerName is the InstrumentationScope name for spans emitted by cmd/sloff
// (the `sloff.run` / `sloff.graph` root spans and `spec.discover`). Callers
// derive a Tracer from the TracerProvider that setupTracing returns.
const cmdTracerName = "github.com/izumin5210/sloff/cmd/sloff"

// consoleSpanWriter is the destination stdouttrace writes to when
// OTEL_TRACES_EXPORTER=console. It must be os.Stderr (not os.Stdout) so
// trace JSON does not interleave with machine-readable subcommand output —
// `sloff graph` writes Mermaid/DOT to stdout and would be unparseable if
// span records were mixed in. Kept as a package-level var so the contract
// is grep-able and test-assertable.
var consoleSpanWriter io.Writer = os.Stderr

// endSpan finishes span with error status when *errp is non-nil. The pointer
// indirection lets callers tie span outcome to a named return value:
//
//	defer endSpan(span, &err)
func endSpan(span trace.Span, errp *error) {
	if errp != nil && *errp != nil {
		span.RecordError(*errp)
		span.SetStatus(codes.Error, (*errp).Error())
	}
	span.End()
}

// otelEnvKeys is the set of OTEL_* env vars sloff lets users override via the
// SLOFF_OTEL_* prefix (ADR-0009 D2'). It is consulted purely as data — sloff
// never writes any of these keys via os.Setenv. Subprocesses spawned by runner
// therefore inherit only the OTEL_* values the user's shell already set.
var otelEnvKeys = []string{
	"OTEL_EXPORTER_OTLP_ENDPOINT",
	"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	"OTEL_EXPORTER_OTLP_HEADERS",
	"OTEL_EXPORTER_OTLP_TRACES_HEADERS",
	"OTEL_EXPORTER_OTLP_PROTOCOL",
	"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
	"OTEL_TRACES_EXPORTER",
	"OTEL_SERVICE_NAME",
	"OTEL_RESOURCE_ATTRIBUTES",
	"OTEL_SDK_DISABLED",
}

// effectiveEnv returns the SLOFF_-prefixed value for an OTEL_ key when set,
// otherwise the OTEL_ value. The whole tracing setup is built on this read-
// only primitive so that SLOFF_ overrides can be honored without ever calling
// os.Setenv — subprocesses spawned by runner therefore never inherit
// SLOFF_-derived OTEL_* values.
//
// LookupEnv on the SLOFF_ key distinguishes "set to empty" from "unset", so an
// explicit `SLOFF_OTEL_TRACES_EXPORTER=""` blanks the effective value rather
// than falling through to OTEL_TRACES_EXPORTER.
func effectiveEnv(otelKey string) string {
	if v, ok := os.LookupEnv("SLOFF_" + otelKey); ok {
		return v
	}
	return os.Getenv(otelKey)
}

// firstNonEmpty returns the first effective value among keys that is not "".
// Used to honor signal-specific OTEL_EXPORTER_OTLP_TRACES_X taking precedence
// over generic OTEL_EXPORTER_OTLP_X (the OTel spec rule).
func firstNonEmpty(keys ...string) string {
	for _, k := range keys {
		if v := effectiveEnv(k); v != "" {
			return v
		}
	}
	return ""
}

// envOTelEnabled reports whether the user has expressed intent to export traces.
// All reads go through effectiveEnv so SLOFF_-prefix overrides participate
// without any process-env mutation.
//
// Disable signals win over enable signals so a SLOFF_-targeted opt-out can
// silence sloff even when the surrounding shell sets a generic OTLP endpoint:
//
//   - OTEL_SDK_DISABLED=true forces disabled
//   - OTEL_TRACES_EXPORTER=none forces disabled
//
// Otherwise, any of the OTLP endpoint vars or a non-"none" OTEL_TRACES_EXPORTER
// being set to a non-empty value enables tracing.
func envOTelEnabled() bool {
	if strings.EqualFold(effectiveEnv("OTEL_SDK_DISABLED"), "true") {
		return false
	}
	if strings.EqualFold(effectiveEnv("OTEL_TRACES_EXPORTER"), "none") {
		return false
	}
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_TRACES_EXPORTER",
	} {
		if effectiveEnv(k) != "" {
			return true
		}
	}
	return false
}

// parseOTLPHeaders parses the OTel OTLP headers env value
// ("k1=v1,k2=v2") into a map. Values are percent-decoded per the OTel spec so
// users can embed commas or equals in header values via percent-escapes.
//
// url.PathUnescape (rather than url.QueryUnescape) is mandatory: query-style
// decoding rewrites '+' to space, which silently corrupts base64-encoded auth
// tokens (the common case for `Authorization: Bearer <base64>` headers).
//
// Malformed pairs return an error rather than being silently dropped, since
// "wrong auth header" tends to be the kind of misconfiguration users want to
// see immediately.
func parseOTLPHeaders(s string) (map[string]string, error) {
	if s == "" {
		return nil, nil
	}
	out := make(map[string]string)
	for part := range strings.SplitSeq(s, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("invalid OTLP header pair %q (expected key=value)", part)
		}
		key := strings.TrimSpace(kv[0])
		if key == "" {
			return nil, fmt.Errorf("empty key in OTLP header pair %q", part)
		}
		val, err := url.PathUnescape(strings.TrimSpace(kv[1]))
		if err != nil {
			return nil, fmt.Errorf("decode OTLP header value for %q: %w", key, err)
		}
		out[key] = val
	}
	return out, nil
}

// parseResourceAttributes parses OTEL_RESOURCE_ATTRIBUTES
// ("k1=v1,k2=v2") into attribute.KeyValue list. Values are percent-decoded
// via url.PathUnescape so '+' stays literal (deployment ids, version
// qualifiers like "1.0+local", and similar values commonly carry it).
//
// Malformed pairs are skipped silently to mirror the leniency of the OTel
// SDK's resource.WithFromEnv (resource attributes are diagnostic metadata,
// not auth-critical, so partial success is preferable to startup failure).
func parseResourceAttributes(s string) []attribute.KeyValue {
	if s == "" {
		return nil
	}
	var out []attribute.KeyValue
	for part := range strings.SplitSeq(s, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		if key == "" {
			continue
		}
		val, err := url.PathUnescape(strings.TrimSpace(kv[1]))
		if err != nil {
			continue
		}
		out = append(out, attribute.String(key, val))
	}
	return out
}

// buildResource constructs the OTel Resource entirely from effective env, with
// no resource.WithFromEnv. service.name / service.version come from
// OTEL_SERVICE_NAME (defaulting to "sloff") and the build-injected
// buildVersion; OTEL_RESOURCE_ATTRIBUTES is parsed by parseResourceAttributes.
// Process / OS / host detectors are still pulled in — they read system info,
// not OTEL_ env, so they cannot leak SLOFF_ values into anything.
func buildResource(ctx context.Context) (*resource.Resource, error) {
	serviceName := effectiveEnv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "sloff"
	}
	attrs := []attribute.KeyValue{
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(buildVersion),
	}
	attrs = append(attrs, parseResourceAttributes(effectiveEnv("OTEL_RESOURCE_ATTRIBUTES"))...)
	return resource.New(
		ctx,
		resource.WithProcess(),
		resource.WithOS(),
		resource.WithHost(),
		resource.WithAttributes(attrs...),
	)
}

// sloffSupportsTracesExporter reports whether the given OTEL_TRACES_EXPORTER
// value names an exporter sloff implements. "" defaults to otlp per the OTel
// spec. Used by setupTracing's spec-compliant noop fallback for unknown
// values; see the buildSpanExporter switch for the matching dispatch.
func sloffSupportsTracesExporter(value string) bool {
	switch strings.ToLower(value) {
	case "", "otlp", "console":
		return true
	default:
		return false
	}
}

// buildSpanExporter dispatches to otlptracehttp / otlptracegrpc / stdouttrace
// based on effective env, replacing autoexport so that endpoint / headers /
// protocol come in via explicit options rather than via os.Setenv-driven env.
// Options always win over env reads inside the OTel exporter constructors, so
// a SLOFF_-overridden value lands on the exporter even though we never wrote
// it back to OTEL_*.
func buildSpanExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	switch strings.ToLower(effectiveEnv("OTEL_TRACES_EXPORTER")) {
	case "console":
		return stdouttrace.New(stdouttrace.WithWriter(consoleSpanWriter))
	case "", "otlp":
		return buildOTLPSpanExporter(ctx)
	default:
		return nil, fmt.Errorf("otel: unsupported OTEL_TRACES_EXPORTER=%q (supported: otlp, console, none)", effectiveEnv("OTEL_TRACES_EXPORTER"))
	}
}

func buildOTLPSpanExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	protocol := firstNonEmpty("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_PROTOCOL")
	if protocol == "" {
		protocol = "http/protobuf"
	}
	switch protocol {
	case "grpc":
		return otlptracegrpc.New(ctx, grpcSpanExporterOpts()...)
	case "http/protobuf":
		return otlptracehttp.New(ctx, httpSpanExporterOpts()...)
	case "http/json":
		// OTel spec advertises http/json, but otel-go's otlptracehttp emits
		// OTLP/protobuf bytes regardless of this setting. Silently sending
		// protobuf when the user asked for JSON would break collectors that
		// expect JSON; reject upfront with a clear message instead.
		return nil, fmt.Errorf("otel: OTLP protocol %q is not implemented by otel-go's otlptracehttp; use http/protobuf or grpc", protocol)
	default:
		return nil, fmt.Errorf("otel: unsupported OTLP protocol %q (supported: grpc, http/protobuf)", protocol)
	}
}

// resolveOTLPHTTPEndpoint computes the full URL the OTLP/HTTP exporter
// should POST traces to, following the OTel env-config spec:
//
//   - OTEL_EXPORTER_OTLP_TRACES_ENDPOINT (signal-specific) is taken as-is.
//   - OTEL_EXPORTER_OTLP_ENDPOINT (generic) is a base URL; the traces
//     signal MUST append "/v1/traces" to it.
//
// Returns "" when neither is set so the caller can omit WithEndpointURL and
// let otlptracehttp default to its built-in localhost endpoint.
//
// Without this append, the most common HTTP setup
// (`OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4318`) would POST to "/"
// and most collectors would reject the export.
func resolveOTLPHTTPEndpoint() string {
	if v := effectiveEnv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"); v != "" {
		return v
	}
	if v := effectiveEnv("OTEL_EXPORTER_OTLP_ENDPOINT"); v != "" {
		return strings.TrimRight(v, "/") + "/v1/traces"
	}
	return ""
}

func httpSpanExporterOpts() []otlptracehttp.Option {
	var opts []otlptracehttp.Option
	if endpoint := resolveOTLPHTTPEndpoint(); endpoint != "" {
		opts = append(opts, otlptracehttp.WithEndpointURL(endpoint))
	}
	if rawHeaders := firstNonEmpty("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_EXPORTER_OTLP_HEADERS"); rawHeaders != "" {
		// Header parse failure is logged-and-swallowed: a malformed
		// SLOFF_OTEL_*_HEADERS shouldn't sink the whole sloff run. The exporter
		// still gets built without headers; users that depend on auth headers
		// will see export errors and can fix the env value.
		if hdrs, err := parseOTLPHeaders(rawHeaders); err == nil && len(hdrs) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(hdrs))
		} else if err != nil {
			fmt.Fprintf(os.Stderr, "sloff: ignoring malformed OTLP headers: %v\n", err)
		}
	}
	return opts
}

func grpcSpanExporterOpts() []otlptracegrpc.Option {
	var opts []otlptracegrpc.Option
	if endpoint := firstNonEmpty("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT"); endpoint != "" {
		opts = append(opts, otlptracegrpc.WithEndpointURL(endpoint))
	}
	if rawHeaders := firstNonEmpty("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_EXPORTER_OTLP_HEADERS"); rawHeaders != "" {
		if hdrs, err := parseOTLPHeaders(rawHeaders); err == nil && len(hdrs) > 0 {
			opts = append(opts, otlptracegrpc.WithHeaders(hdrs))
		} else if err != nil {
			fmt.Fprintf(os.Stderr, "sloff: ignoring malformed OTLP headers: %v\n", err)
		}
	}
	return opts
}

// setupTracing builds a sloff-local TracerProvider from env signals. It
// **never touches** the otel-go global TracerProvider or TextMapPropagator,
// so concurrent invocations in the same process are safe and host code that
// has its own global stays untouched.
//
// **Disabled** (OTEL_SDK_DISABLED=true, OTEL_TRACES_EXPORTER=none, or simply
// no tracing env at all): returns a noop TracerProvider. cmd/sloff and
// runner derive their tracers from this and emit nothing.
//
// **Enabled**: builds the Resource and SpanExporter from effective env
// (`SLOFF_OTEL_*` overrides win over `OTEL_*` via effectiveEnv) using
// explicit options on the OTel SDK constructors. Returns the configured
// TracerProvider so the caller can pass it to runner.Options.TracerProvider
// and derive a Tracer for the cmd/sloff root spans.
//
// **Env immutability**: setupTracing never calls os.Setenv. Subprocesses
// spawned by runner.Run therefore inherit only the OTEL_* values the user's
// shell already set; SLOFF_-derived values never leak to codegen tools (and
// runner additionally strips SLOFF_OTEL_* from the subprocess env).
//
// The returned shutdown is always non-nil and safe to call once.
func setupTracing(ctx context.Context) (trace.TracerProvider, func(context.Context) error, error) {
	if !envOTelEnabled() {
		// Both the explicit-disable and passive-disable paths land here.
		// Return a noop sloff-local provider; tracers derived from it emit
		// nothing, so sloff is silent regardless of any host TracerProvider.
		return noop.NewTracerProvider(), func(context.Context) error { return nil }, nil
	}

	// Spec compliance: OTEL_TRACES_EXPORTER values sloff doesn't implement
	// (zipkin, jaeger, comma-separated multi-export, ...) MUST warn and fall
	// back to noop, not fail. Otherwise a shell that exports a non-OTLP
	// exporter for other tools would brick `sloff run` / `graph` for the same
	// user. SLOFF_OTEL_TRACES_EXPORTER is the escape hatch when the user
	// wants sloff to export OTLP regardless of the shell-wide value.
	if exp := effectiveEnv("OTEL_TRACES_EXPORTER"); !sloffSupportsTracesExporter(exp) {
		fmt.Fprintf(os.Stderr,
			"sloff: OTEL_TRACES_EXPORTER=%q is not implemented by sloff (supported: otlp, console); sloff tracing disabled. Set SLOFF_OTEL_TRACES_EXPORTER=otlp (or =console) to override.\n",
			exp)
		return noop.NewTracerProvider(), func(context.Context) error { return nil }, nil
	}

	res, err := buildResource(ctx)
	// resource.ErrPartialResource is returned when one detector fails but the rest
	// produced usable attributes; partial resource info is preferable to aborting
	// tracing setup, so only hard-fail on non-partial errors.
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, nil, fmt.Errorf("otel: build resource: %w", err)
	}

	exp, err := buildSpanExporter(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("otel: build span exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exp),
	)
	return tp, tp.Shutdown, nil
}
