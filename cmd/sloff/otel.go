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

// envSnapshot captures the original state of one OTEL_ key so a later restore
// can revert to "set to v" or "not set", matching the pre-override observable
// state.
type envSnapshot struct {
	key       string
	wasSet    bool
	prevValue string
}

// applySloffPrefixOverrides copies SLOFF_<key> into <key> for every key sloff
// honors and returns a func that reverts those mutations. SLOFF_-prefixed
// values take precedence so callers can target sloff independently of any
// shell-wide OTEL_* defaults set for other tools.
//
// The returned restore func is mandatory in any in-process scenario (tests,
// embedding hosts that re-invoke the command): without it the SLOFF_-derived
// values leak into later invocations whose env was supposed to behave like a
// fresh shell. CLI one-shot processes never observe the leak, but the contract
// stays the same so call sites do not have to special-case based on host
// behaviour.
//
// LookupEnv distinguishes "set to empty" from "unset" — the explicit empty case
// is preserved so `SLOFF_OTEL_TRACES_EXPORTER=""` blanks the underlying value
// (envOTelEnabled treats empty as disabled), letting users opt sloff out of an
// inherited shell endpoint with a single export.
func applySloffPrefixOverrides() (restore func()) {
	var saved []envSnapshot
	for _, k := range otelEnvKeys {
		v, ok := os.LookupEnv("SLOFF_" + k)
		if !ok {
			continue
		}
		origVal, origSet := os.LookupEnv(k)
		saved = append(saved, envSnapshot{key: k, wasSet: origSet, prevValue: origVal})
		os.Setenv(k, v)
	}
	return func() {
		for _, e := range saved {
			if e.wasSet {
				os.Setenv(e.key, e.prevValue)
			} else {
				os.Unsetenv(e.key)
			}
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

// setupTracing wires the global TracerProvider when env signals export intent.
//
// On the **disabled** path the global provider is left untouched: in-process
// hosts that already configured OpenTelemetry (e.g. tests, embedding programs)
// keep their tracer wiring, and sloff's runner spans flow through whatever the
// host has set up. Only the SLOFF_-prefix env mutations are reverted on
// shutdown.
//
// On the **enabled** path sloff installs its own TracerProvider and propagator
// so the spans land on the configured OTLP exporter (regardless of what host
// state existed). The shutdown drains the BatchSpanProcessor and restores the
// caller's prior provider + propagator + env so the host process can keep
// running afterwards without a shut-down provider stuck on the global.
//
// The returned shutdown is always non-nil and safe to call once.
func setupTracing(ctx context.Context) (func(context.Context) error, error) {
	restoreEnv := applySloffPrefixOverrides()
	if !envOTelEnabled() {
		return func(context.Context) error {
			restoreEnv()
			return nil
		}, nil
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
		restoreEnv()
		return nil, fmt.Errorf("otel: build resource: %w", err)
	}

	exp, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		restoreEnv()
		return nil, fmt.Errorf("otel: build span exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exp),
	)
	prevProvider := otel.GetTracerProvider()
	prevPropagator := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return func(ctx context.Context) error {
		err := tp.Shutdown(ctx)
		otel.SetTracerProvider(prevProvider)
		otel.SetTextMapPropagator(prevPropagator)
		restoreEnv()
		return err
	}, nil
}
