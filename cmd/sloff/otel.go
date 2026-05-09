package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// cmdTracer is the package-level Tracer for cmd/sloff. otel.Tracer is wired to
// the global provider lazily, so creating it at package init is safe even though
// setupTracing replaces the provider later.
var cmdTracer = otel.Tracer("github.com/izumin5210/sloff/cmd/sloff")

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
// SLOFF_OTEL_* prefix. The list intentionally covers both enable-detection keys
// and resource/exporter keys so a single SLOFF_-prefixed value can fully retarget
// or silence sloff's tracing without touching the surrounding shell. ADR-0009 D2'.
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

// applySloffPrefixOverrides copies SLOFF_<key> into <key> for every key sloff
// honors. SLOFF_-prefixed values take precedence so callers can target sloff
// independently of any shell-wide OTEL_* defaults set for other tools.
//
// LookupEnv distinguishes "set to empty" from "unset" — the explicit empty case
// is preserved so `SLOFF_OTEL_TRACES_EXPORTER=""` blanks the underlying value
// (envOTelEnabled treats empty as disabled), letting users opt sloff out of an
// inherited shell endpoint with a single export.
func applySloffPrefixOverrides() {
	for _, k := range otelEnvKeys {
		if v, ok := os.LookupEnv("SLOFF_" + k); ok {
			os.Setenv(k, v)
		}
	}
}

// envOTelEnabled reports whether the user has expressed intent to export traces.
// Callers must invoke applySloffPrefixOverrides first so SLOFF_-prefixed overrides
// participate.
//
// Disable signals win over enable signals so a SLOFF_-targeted opt-out can silence
// sloff even when the surrounding shell sets a generic OTLP endpoint:
//
//   - OTEL_SDK_DISABLED=true forces disabled
//   - OTEL_TRACES_EXPORTER=none forces disabled
//
// Otherwise, any of the OTLP endpoint vars or a non-"none" OTEL_TRACES_EXPORTER
// being set to a non-empty value enables tracing.
func envOTelEnabled() bool {
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return false
	}
	if strings.EqualFold(os.Getenv("OTEL_TRACES_EXPORTER"), "none") {
		return false
	}
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_TRACES_EXPORTER",
	} {
		if v := os.Getenv(k); v != "" {
			return true
		}
	}
	return false
}

// setupTracing wires the global TracerProvider when env signals export intent,
// otherwise installs an explicit no-op provider so package-level otel.Tracer
// calls scattered across sloff land on a known zero-cost provider regardless of
// what any prior process has set globally.
//
// Returns a shutdown func the caller MUST defer. The shutdown is always safe to
// call (no-op when disabled) and on the enabled path drains the BatchSpanProcessor
// — short-lived CLI processes cannot afford to lose the final span batch.
func setupTracing(ctx context.Context) (func(context.Context) error, error) {
	applySloffPrefixOverrides()
	if !envOTelEnabled() {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}

	res, err := resource.New(
		ctx,
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithOS(),
		resource.WithHost(),
		resource.WithAttributes(
			semconv.ServiceName("sloff"),
			semconv.ServiceVersion(buildVersion),
		),
	)
	// resource.ErrPartialResource is returned when one detector fails but the rest
	// produced usable attributes; partial resource info is preferable to aborting
	// tracing setup, so only hard-fail on non-partial errors.
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, fmt.Errorf("otel: build resource: %w", err)
	}

	exp, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		return nil, fmt.Errorf("otel: build span exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exp),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}
