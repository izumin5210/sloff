package runner_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint/local"
	"github.com/izumin5210/sloff/internal/sloff/preflight"
	"github.com/izumin5210/sloff/internal/sloff/runner"
	"github.com/izumin5210/sloff/internal/sloff/spec"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/golocal"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/lister"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/script"
)

// These E2E cases cover ADR-0015 (dynamic tasks via command_providers). The
// fixtures under testdata/e2e/runner/provider-*/ carry a gen.sh that emits one
// copy task per *.src file, so the task set is derived from the filesystem
// rather than hand-written.

// TestRunner_Provider_FirstRunWritesRecords: every generated task runs and
// persists a record under its generated name (.sloff/fingerprints/spec/copy-<x>/).
func TestRunner_Provider_FirstRunWritesRecords(t *testing.T) {
	requireSh(t)
	runE2E(t, "provider-first-run-writes-records", runStep())
}

// TestRunner_Provider_SecondRunHits: the provider re-execs on the warm run and
// re-emits the same task set, but every generated task is a fingerprint hit
// (output-comparison), so the tree — records included — is byte-identical.
func TestRunner_Provider_SecondRunHits(t *testing.T) {
	requireSh(t)
	runE2E(t, "provider-second-run-hits", runStep(), runStep())
}

// TestRunner_Provider_TaskSetChanges: adding a source file grows the set the
// provider emits. The new task runs; the unchanged ones stay fingerprint hits.
// Demonstrates filesystem-derived fan-out (ADR-0015 axis A).
func TestRunner_Provider_TaskSetChanges(t *testing.T) {
	requireSh(t)
	runE2E(
		t, "provider-task-set-changes",
		runStep(),
		writeStep("spec/c.src", "charlie\n"),
		runStep(),
	)
}

// TestRunner_Provider_LogicChangeReemits proves ADR-0015 D3: changing the
// provider so it emits a different cmd for one task invalidates that task via
// the ordinary cmd_hash path — no provider-version injection is involved. The
// edited gen.sh makes copy-a append "v2" to its output; copy-b is unchanged and
// stays a hit (a second copy-a record appears alongside the original, the same
// way every other invalidate-by-input case accumulates records).
func TestRunner_Provider_LogicChangeReemits(t *testing.T) {
	requireSh(t)
	runE2E(
		t, "provider-logic-change-reemits",
		runStep(),
		writeStep("spec/gen.sh", providerGenV2),
		runStep(),
	)
}

// providerGenV2 is the edited gen.sh dropped in by the logic-change test: copy-a
// now appends "v2" to its output (a different cmd), while every other source
// keeps the plain copy cmd.
const providerGenV2 = `printf '{"schema_version":"v1","tasks":['
sep=
for f in *.src; do
  [ -e "$f" ] || continue
  name=${f%.src}
  if [ "$name" = a ]; then
    printf '%s{"name":"copy-%s","cmd":["sh","-c","cp %s %s.txt; printf v2 >> %s.txt"],"inputs":["%s"],"outputs":["%s.txt"],"tools":["versioner"]}' "$sep" "$name" "$f" "$name" "$name" "$f" "$name"
  else
    printf '%s{"name":"copy-%s","cmd":["cp","%s","%s.txt"],"inputs":["%s"],"outputs":["%s.txt"],"tools":["versioner"]}' "$sep" "$name" "$f" "$name" "$f" "$name"
  fi
  sep=,
done
printf ']}'
`

func TestRunner_Provider_ExecFailureAborts(t *testing.T) {
	workdir, specs := setupProviderWorkdir(t, map[string]string{
		"spec/sloff.yml": providerYML(`["sh", "gen.sh"]`),
		"spec/gen.sh":    "echo boom >&2; exit 5\n",
	})
	err := newProviderRunner(t, workdir, specs).Run(context.Background())
	if err == nil {
		t.Fatal("expected error from a failing command provider")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should surface the provider's stderr, got: %v", err)
	}
}

func TestRunner_Provider_BadSchemaVersionFails(t *testing.T) {
	workdir, specs := setupProviderWorkdir(t, map[string]string{
		"spec/sloff.yml": providerYML(`["sh", "gen.sh"]`),
		"spec/gen.sh":    `printf '{"schema_version":"v2","tasks":[]}'` + "\n",
	})
	err := newProviderRunner(t, workdir, specs).Run(context.Background())
	if err == nil {
		t.Fatal("expected error for unsupported schema_version")
	}
	if !strings.Contains(err.Error(), "schema_version") {
		t.Errorf("error should mention schema_version, got: %v", err)
	}
}

// TestRunner_Provider_OutputConflictFails: two generated tasks declaring the
// same output is a spec conflict the existing ADR-0004 D3 detector catches once
// the generated commands flow through collectTasks unchanged (ADR-0015 D5).
func TestRunner_Provider_OutputConflictFails(t *testing.T) {
	gen := `printf '{"schema_version":"v1","tasks":[` +
		`{"name":"t1","cmd":["true"],"inputs":["in"],"outputs":["shared.out"],"tools":["versioner"]},` +
		`{"name":"t2","cmd":["true"],"inputs":["in"],"outputs":["shared.out"],"tools":["versioner"]}]}'` + "\n"
	workdir, specs := setupProviderWorkdir(t, map[string]string{
		"spec/sloff.yml": providerYML(`["sh", "gen.sh"]`),
		"spec/gen.sh":    gen,
	})
	err := newProviderRunner(t, workdir, specs).Run(context.Background())
	if err == nil {
		t.Fatal("expected error when two generated tasks share an output")
	}
	if !strings.Contains(err.Error(), "shared.out") {
		t.Errorf("error should name the conflicting output, got: %v", err)
	}
}

// TestRunner_Provider_NameCollisionWithStaticFails: a generated task name that
// collides with a hand-written command is rejected by the same validation
// static commands face, run on the merged set (ADR-0015 D5).
func TestRunner_Provider_NameCollisionWithStaticFails(t *testing.T) {
	yml := `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: dup
    cmd: ["true"]
    inputs: ["in"]
    outputs: ["static.out"]
    tools: [versioner]

command_providers:
  - name: gen
    exec: ["sh", "gen.sh"]
`
	gen := `printf '{"schema_version":"v1","tasks":[{"name":"dup","cmd":["true"],"inputs":["in"],"outputs":["gen.out"],"tools":["versioner"]}]}'` + "\n"
	workdir, specs := setupProviderWorkdir(t, map[string]string{
		"spec/sloff.yml": yml,
		"spec/gen.sh":    gen,
	})
	err := newProviderRunner(t, workdir, specs).Run(context.Background())
	if err == nil {
		t.Fatal("expected error when a generated task name collides with a static one")
	}
	if !strings.Contains(err.Error(), "dup") {
		t.Errorf("error should name the colliding task, got: %v", err)
	}
}

func requireSh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
}

// providerYML renders a minimal sloff.yml whose only task source is one command
// provider; execArr is the YAML flow array for its exec field.
func providerYML(execArr string) string {
	return `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

command_providers:
  - name: gen
    exec: ` + execArr + "\n"
}

func setupProviderWorkdir(t *testing.T, files map[string]string) (string, []spec.Spec) {
	t.Helper()
	requireSh(t)
	workdir := t.TempDir()
	gitInitWorkdir(t, workdir)
	for rel, content := range files {
		full := filepath.Join(workdir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	specs, err := spec.Discover(workdir, "**/sloff.yml")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	return workdir, specs
}

func newProviderRunner(t *testing.T, workdir string, specs []spec.Spec) *runner.Runner {
	t.Helper()
	reg := toolresolver.NewRegistry()
	reg.Register(script.New(workdir))
	reg.Register(golocal.New(workdir, lister.NewMemoized(lister.NewGoPackages(workdir))))
	return runner.New(runner.Options{
		RepoRoot:  workdir,
		Specs:     specs,
		Storage:   local.New(workdir, local.WithClock(func() time.Time { return fixedClock })),
		Resolvers: reg,
		Preflight: preflight.NewRegistry(),
	})
}
