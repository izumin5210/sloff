package spec_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/lazygen/internal/lazygen/spec"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    *spec.File
		wantErr bool
	}{
		{
			name: "tools and commands together",
			yaml: `tools:
  buf:
    exec: ["buf", "--version"]
  protoc-gen-foo:
    go-local: ./cmd/protoc-gen-foo
  codegen:
    pnpm-local: '@org/codegen'

commands:
  - name: gen
    cmd: buf generate
    inputs: ["**/*.proto"]
    outputs: ["**/*.pb.go"]
    tools: [buf, protoc-gen-foo]
`,
			want: &spec.File{
				Tools: map[string]spec.DeclaredTool{
					"buf":            {Resolver: "script", Exec: []string{"buf", "--version"}},
					"protoc-gen-foo": {Resolver: "go-local", Entry: "./cmd/protoc-gen-foo"},
					"codegen":        {Resolver: "pnpm-local", PackageName: "@org/codegen"},
				},
				Commands: []spec.Command{{
					Name:    "gen",
					Cmd:     []string{"buf", "generate"},
					Inputs:  []string{"**/*.proto"},
					Outputs: []string{"**/*.pb.go"},
					Tools:   []string{"buf", "protoc-gen-foo"},
				}},
			},
		},
		{
			name: "tools-only file (catalog)",
			yaml: `tools:
  buf:
    exec: ["buf", "--version"]
`,
			want: &spec.File{
				Tools: map[string]spec.DeclaredTool{
					"buf": {Resolver: "script", Exec: []string{"buf", "--version"}},
				},
			},
		},
		{
			name: "commands-only file (references tools defined elsewhere)",
			yaml: `commands:
  - name: gen
    cmd: foo
    inputs: ["a"]
    outputs: ["b"]
    tools: [external-tool]
`,
			want: &spec.File{
				Commands: []spec.Command{{
					Name:    "gen",
					Cmd:     []string{"foo"},
					Inputs:  []string{"a"},
					Outputs: []string{"b"},
					Tools:   []string{"external-tool"},
				}},
			},
		},
		{
			name: "command with cmd as list",
			yaml: `tools:
  foo:
    exec: ["foo", "--version"]
commands:
  - name: gen
    cmd: ["foo", "bar baz"]
    inputs: ["a"]
    outputs: ["b"]
    tools: [foo]
`,
			want: &spec.File{
				Tools: map[string]spec.DeclaredTool{
					"foo": {Resolver: "script", Exec: []string{"foo", "--version"}},
				},
				Commands: []spec.Command{{
					Name:    "gen",
					Cmd:     []string{"foo", "bar baz"},
					Inputs:  []string{"a"},
					Outputs: []string{"b"},
					Tools:   []string{"foo"},
				}},
			},
		},
		{
			name: "duplicate task name fails",
			yaml: `tools:
  foo: {exec: ["foo", "--version"]}
commands:
  - name: gen
    cmd: foo
    inputs: ["a"]
    outputs: ["b"]
    tools: [foo]
  - name: gen
    cmd: bar
    inputs: ["c"]
    outputs: ["d"]
    tools: [foo]
`,
			wantErr: true,
		},
		{
			name: "missing name fails",
			yaml: `commands:
  - cmd: foo
    inputs: ["a"]
    outputs: ["b"]
    tools: [foo]
`,
			wantErr: true,
		},
		{
			name: "missing cmd fails",
			yaml: `commands:
  - name: gen
    inputs: ["a"]
    outputs: ["b"]
    tools: [foo]
`,
			wantErr: true,
		},
		{
			name: "missing inputs fails",
			yaml: `commands:
  - name: gen
    cmd: foo
    outputs: ["b"]
    tools: [foo]
`,
			wantErr: true,
		},
		{
			name: "missing outputs fails",
			yaml: `commands:
  - name: gen
    cmd: foo
    inputs: ["a"]
    tools: [foo]
`,
			wantErr: true,
		},
		{
			name: "missing tools fails",
			yaml: `commands:
  - name: gen
    cmd: foo
    inputs: ["a"]
    outputs: ["b"]
`,
			wantErr: true,
		},
		{
			name: "empty tools fails",
			yaml: `commands:
  - name: gen
    cmd: foo
    inputs: ["a"]
    outputs: ["b"]
    tools: []
`,
			wantErr: true,
		},
		{
			name: "empty file (no tools, no commands) fails",
			yaml: `# nothing here
`,
			wantErr: true,
		},
		{
			name: "tool entry without recognized fields fails",
			yaml: `tools:
  bad:
    unknown: x
`,
			wantErr: true,
		},
		{
			name: "tool entry combining exec and go-local fails",
			yaml: `tools:
  bad:
    exec: ["foo", "--version"]
    go-local: ./cmd/foo
`,
			wantErr: true,
		},
		{
			name: "tool entry combining exec and pnpm-local fails",
			yaml: `tools:
  bad:
    exec: ["foo", "--version"]
    pnpm-local: '@org/foo'
`,
			wantErr: true,
		},
		{
			name: "tool entry combining go-local and pnpm-local fails",
			yaml: `tools:
  bad:
    go-local: ./cmd/foo
    pnpm-local: '@org/foo'
`,
			wantErr: true,
		},
		{
			name: "tool name with uppercase fails",
			yaml: `tools:
  Bad:
    exec: ["foo", "--version"]
`,
			wantErr: true,
		},
		{
			name: "tool name starting with hyphen fails",
			yaml: `tools:
  -bad:
    exec: ["foo", "--version"]
`,
			wantErr: true,
		},
		{
			name: "go-local entry without leading dot-slash fails",
			yaml: `tools:
  bad:
    go-local: cmd/foo
`,
			wantErr: true,
		},
		{
			name: "go-local entry of bare . is accepted",
			yaml: `tools:
  gen:
    go-local: .
commands:
  - name: gen
    cmd: go run .
    inputs: ["a"]
    outputs: ["b"]
    tools: [gen]
`,
			want: &spec.File{
				Tools: map[string]spec.DeclaredTool{
					"gen": {Resolver: "go-local", Entry: "."},
				},
				Commands: []spec.Command{{
					Name:    "gen",
					Cmd:     []string{"go", "run", "."},
					Inputs:  []string{"a"},
					Outputs: []string{"b"},
					Tools:   []string{"gen"},
				}},
			},
		},
		{
			name: "go-local parent-relative entry is accepted",
			yaml: `tools:
  gen:
    go-local: ../cmd/gen
commands:
  - name: gen
    cmd: go run ../cmd/gen
    inputs: ["a"]
    outputs: ["b"]
    tools: [gen]
`,
			want: &spec.File{
				Tools: map[string]spec.DeclaredTool{
					"gen": {Resolver: "go-local", Entry: "../cmd/gen"},
				},
				Commands: []spec.Command{{
					Name:    "gen",
					Cmd:     []string{"go", "run", "../cmd/gen"},
					Inputs:  []string{"a"},
					Outputs: []string{"b"},
					Tools:   []string{"gen"},
				}},
			},
		},
		{
			name: "empty tool name reference fails",
			yaml: `tools:
  foo: {exec: ["foo", "--version"]}
commands:
  - name: gen
    cmd: foo
    inputs: ["a"]
    outputs: ["b"]
    tools: ["", foo]
`,
			wantErr: true,
		},
		{
			name:    "invalid yaml",
			yaml:    `commands: [unbalanced`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := spec.Parse([]byte(tt.yaml))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a", "lazygen.yml"), `tools:
  do-a:
    exec: ["do-a", "--version"]
commands:
  - name: alpha
    cmd: do-a
    inputs: ["**/*.in"]
    outputs: ["**/*.out"]
    tools: [do-a]
`)
	mustWrite(t, filepath.Join(root, "nested", "b", "lazygen.yml"), `tools:
  do-b:
    exec: ["do-b", "--version"]
commands:
  - name: beta
    cmd: do-b
    inputs: ["**/*.in"]
    outputs: ["**/*.out"]
    tools: [do-b]
`)
	mustWrite(t, filepath.Join(root, "ignored", "other.yml"), "irrelevant")

	got, err := spec.Discover(root, "**/lazygen.yml")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	sort.Slice(got, func(i, j int) bool { return got[i].Dir < got[j].Dir })

	if len(got) != 2 {
		t.Fatalf("len(got)=%d, want 2: %#v", len(got), got)
	}
	if got[0].Dir != "a" || got[0].File.Commands[0].Name != "alpha" {
		t.Errorf("got[0]=%+v", got[0])
	}
	if got[1].Dir != filepath.Join("nested", "b") || got[1].File.Commands[0].Name != "beta" {
		t.Errorf("got[1]=%+v", got[1])
	}
}

// TestDiscover_DuplicateTaskAcrossSpecsAllowed guards that the same task name
// can appear in different spec dirs (they're disambiguated by spec dir in the
// cache record path), even though duplicates within one file are rejected.
func TestDiscover_DuplicateTaskAcrossSpecsAllowed(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a", "lazygen.yml"), `tools:
  foo: {exec: ["foo", "--version"]}
commands:
  - name: gen
    cmd: foo
    inputs: ["a.in"]
    outputs: ["a.out"]
    tools: [foo]
`)
	mustWrite(t, filepath.Join(root, "b", "lazygen.yml"), `tools:
  bar: {exec: ["bar", "--version"]}
commands:
  - name: gen
    cmd: bar
    inputs: ["b.in"]
    outputs: ["b.out"]
    tools: [bar]
`)
	got, err := spec.Discover(root, "**/lazygen.yml")
	if err != nil {
		t.Fatalf("same name in different spec dirs should not error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got)=%d, want 2", len(got))
	}
}

// TestBuildToolRegistry_MergesAcrossFiles guards the ADR-0008 D2 invariant:
// tool names live in a flat repo-wide namespace built by walking every
// discovered lazygen.yml. Lookup is by short name regardless of where the
// tool was defined.
func TestBuildToolRegistry_MergesAcrossFiles(t *testing.T) {
	specs := []spec.Spec{
		{Dir: "proto", File: &spec.File{Tools: map[string]spec.DeclaredTool{
			"buf":            {Resolver: "script", Exec: []string{"buf", "--version"}},
			"protoc-gen-foo": {Resolver: "go-local", Entry: "./cmd/protoc-gen-foo"},
		}}},
		{Dir: "api", File: &spec.File{Tools: map[string]spec.DeclaredTool{
			"codegen": {Resolver: "pnpm-local", PackageName: "@org/codegen"},
		}}},
	}
	reg, err := spec.BuildToolRegistry(specs)
	if err != nil {
		t.Fatalf("BuildToolRegistry: %v", err)
	}
	got, ok := reg.Lookup("protoc-gen-foo")
	if !ok {
		t.Fatal("protoc-gen-foo not in registry")
	}
	if got.SpecDir != "proto" {
		t.Errorf("SpecDir = %q, want proto (definition-site dir)", got.SpecDir)
	}
}

// TestBuildToolRegistry_DuplicateNameFails surfaces the cross-spec collision
// case loudly. Without an explicit error, the registry would silently keep
// whichever tool happened to be visited last and silently drift away from
// what the user wrote in the conflicting file.
func TestBuildToolRegistry_DuplicateNameFails(t *testing.T) {
	specs := []spec.Spec{
		{Dir: "proto", File: &spec.File{Tools: map[string]spec.DeclaredTool{
			"codegen": {Resolver: "pnpm-local", PackageName: "@org/codegen-proto"},
		}}},
		{Dir: "api", File: &spec.File{Tools: map[string]spec.DeclaredTool{
			"codegen": {Resolver: "pnpm-local", PackageName: "@org/codegen-api"},
		}}},
	}
	_, err := spec.BuildToolRegistry(specs)
	if err == nil {
		t.Fatal("expected error on duplicate tool name across files")
	}
	for _, want := range []string{"codegen", "proto", "api"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// TestValidateToolReferences_RejectsUndefinedRef catches the case a task
// references a tool that was never declared. Without it, the runner would
// either crash later or silently emit empty contributions.
func TestValidateToolReferences_RejectsUndefinedRef(t *testing.T) {
	specs := []spec.Spec{
		{Dir: "proto", File: &spec.File{
			Tools: map[string]spec.DeclaredTool{
				"buf": {Resolver: "script", Exec: []string{"buf", "--version"}},
			},
			Commands: []spec.Command{{
				Name: "gen", Cmd: []string{"x"},
				Inputs: []string{"a"}, Outputs: []string{"b"},
				Tools: []string{"buf", "missing-tool"},
			}},
		}},
	}
	reg, err := spec.BuildToolRegistry(specs)
	if err != nil {
		t.Fatalf("BuildToolRegistry: %v", err)
	}
	err = spec.ValidateToolReferences(specs, reg)
	if err == nil {
		t.Fatal("expected error on undefined tool reference")
	}
	for _, want := range []string{"missing-tool", "gen", "proto"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
