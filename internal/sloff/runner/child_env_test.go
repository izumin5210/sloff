package runner

import (
	"reflect"
	"testing"
)

func TestChildEnv_StripsSloffOTel(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"OTEL_EXPORTER_OTLP_ENDPOINT=http://shell-collector:4318",
		"OTEL_TRACES_EXPORTER=otlp",
		// SLOFF_-prefixed values are intentionally scoped to the parent
		// sloff run; any task subprocess (which may itself invoke sloff or
		// another OTel-aware tool that honors the same prefix) must NOT
		// observe these.
		"SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT=http://sloff-only:9999",
		"SLOFF_OTEL_TRACES_EXPORTER=none",
		"SLOFF_OTEL_SERVICE_NAME=sloff-renamed",
		"USER=alice",
	}
	want := []string{
		"PATH=/usr/bin",
		"OTEL_EXPORTER_OTLP_ENDPOINT=http://shell-collector:4318",
		"OTEL_TRACES_EXPORTER=otlp",
		"USER=alice",
	}
	got := childEnv(in)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("childEnv() =\n  %v\nwant\n  %v", got, want)
	}
}

func TestChildEnv_NoMatchesPreservesEverything(t *testing.T) {
	in := []string{"PATH=/usr/bin", "OTEL_EXPORTER_OTLP_ENDPOINT=http://x:4318", "USER=alice"}
	got := childEnv(in)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("childEnv() = %v, want %v (nothing should be stripped)", got, in)
	}
}

func TestChildEnv_EmptyInputReturnsEmpty(t *testing.T) {
	got := childEnv(nil)
	if len(got) != 0 {
		t.Errorf("childEnv(nil) = %v, want empty", got)
	}
}
