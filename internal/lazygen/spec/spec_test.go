package spec_test

import (
	"os"
	"path/filepath"
	"sort"
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
			name: "single command with string cmd",
			yaml: `commands:
  - name: protoc-gen-go
    cmd: buf generate --template buf.gen.yaml
    inputs:
      - "**/*.proto"
      - buf.gen.yaml
    outputs:
      - "**/*.pb.go"
      - "**/*.connect.go"
    tools:
      - exec: ["buf", "--version"]
`,
			want: &spec.File{
				Commands: []spec.Command{{
					Name:    "protoc-gen-go",
					Cmd:     []string{"buf", "generate", "--template", "buf.gen.yaml"},
					Inputs:  []string{"**/*.proto", "buf.gen.yaml"},
					Outputs: []string{"**/*.pb.go", "**/*.connect.go"},
					Tools: []spec.DeclaredTool{
						{Resolver: "script", Exec: []string{"buf", "--version"}},
					},
				}},
			},
		},
		{
			name: "command with cmd as list",
			yaml: `commands:
  - name: gen
    cmd: ["foo", "bar baz"]
    inputs: ["a"]
    outputs: ["b"]
    tools:
      - exec: ["foo", "--version"]
`,
			want: &spec.File{
				Commands: []spec.Command{{
					Name:    "gen",
					Cmd:     []string{"foo", "bar baz"},
					Inputs:  []string{"a"},
					Outputs: []string{"b"},
					Tools: []spec.DeclaredTool{
						{Resolver: "script", Exec: []string{"foo", "--version"}},
					},
				}},
			},
		},
		{
			name: "command with script tools",
			yaml: `commands:
  - name: gen
    cmd: foo
    inputs: ["a"]
    outputs: ["b"]
    tools:
      - exec: ["buf", "--version"]
      - exec: ["go", "version"]
        extract: 'go[0-9]+\.[0-9]+(?:\.[0-9]+)?'
`,
			want: &spec.File{
				Commands: []spec.Command{{
					Name:    "gen",
					Cmd:     []string{"foo"},
					Inputs:  []string{"a"},
					Outputs: []string{"b"},
					Tools: []spec.DeclaredTool{
						{Resolver: "script", Exec: []string{"buf", "--version"}},
						{Resolver: "script", Exec: []string{"go", "version"}, Extract: `go[0-9]+\.[0-9]+(?:\.[0-9]+)?`},
					},
				}},
			},
		},
		{
			name: "duplicate task name fails",
			yaml: `commands:
  - name: gen
    cmd: foo
    inputs: ["a"]
    outputs: ["b"]
  - name: gen
    cmd: bar
    inputs: ["c"]
    outputs: ["d"]
`,
			wantErr: true,
		},
		{
			name: "missing name fails",
			yaml: `commands:
  - cmd: foo
    inputs: ["a"]
    outputs: ["b"]
`,
			wantErr: true,
		},
		{
			name: "missing cmd fails",
			yaml: `commands:
  - name: gen
    inputs: ["a"]
    outputs: ["b"]
`,
			wantErr: true,
		},
		{
			name: "missing inputs fails",
			yaml: `commands:
  - name: gen
    cmd: foo
    outputs: ["b"]
`,
			wantErr: true,
		},
		{
			name: "missing outputs fails",
			yaml: `commands:
  - name: gen
    cmd: foo
    inputs: ["a"]
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
			name: "tools entry without recognized fields fails",
			yaml: `commands:
  - name: gen
    cmd: foo
    inputs: ["a"]
    outputs: ["b"]
    tools:
      - unknown: x
`,
			wantErr: true,
		},
		{
			name: "command with go-local tool",
			yaml: `commands:
  - name: gen
    cmd: ["go", "run", "./cmd/protoc-gen-foo/..."]
    inputs: ["**/*.proto"]
    outputs: ["**/*.pb.go"]
    tools:
      - go-local: ./cmd/protoc-gen-foo/...
`,
			want: &spec.File{
				Commands: []spec.Command{{
					Name:    "gen",
					Cmd:     []string{"go", "run", "./cmd/protoc-gen-foo/..."},
					Inputs:  []string{"**/*.proto"},
					Outputs: []string{"**/*.pb.go"},
					Tools: []spec.DeclaredTool{
						{Resolver: "go-local", Entry: "./cmd/protoc-gen-foo/..."},
					},
				}},
			},
		},
		{
			name: "command mixing script and go-local tools",
			yaml: `commands:
  - name: gen
    cmd: ["go", "run", "./cmd/codegen"]
    inputs: ["**/*.proto"]
    outputs: ["**/*.pb.go"]
    tools:
      - exec: ["go", "version"]
        extract: 'go[0-9]+\.[0-9]+(?:\.[0-9]+)?'
      - go-local: ./cmd/codegen
`,
			want: &spec.File{
				Commands: []spec.Command{{
					Name:    "gen",
					Cmd:     []string{"go", "run", "./cmd/codegen"},
					Inputs:  []string{"**/*.proto"},
					Outputs: []string{"**/*.pb.go"},
					Tools: []spec.DeclaredTool{
						{Resolver: "script", Exec: []string{"go", "version"}, Extract: `go[0-9]+\.[0-9]+(?:\.[0-9]+)?`},
						{Resolver: "go-local", Entry: "./cmd/codegen"},
					},
				}},
			},
		},
		{
			name: "tools entry with both exec and go-local fails",
			yaml: `commands:
  - name: gen
    cmd: foo
    inputs: ["a"]
    outputs: ["b"]
    tools:
      - exec: ["foo", "--version"]
        go-local: ./cmd/foo
`,
			wantErr: true,
		},
		{
			name: "go-local entry without leading dot-slash fails",
			yaml: `commands:
  - name: gen
    cmd: foo
    inputs: ["a"]
    outputs: ["b"]
    tools:
      - go-local: cmd/foo
`,
			wantErr: true,
		},
		{
			name: "go-local entry of bare . is accepted",
			yaml: `commands:
  - name: gen
    cmd: go run .
    inputs: ["a"]
    outputs: ["b"]
    tools:
      - go-local: .
`,
			want: &spec.File{
				Commands: []spec.Command{{
					Name:    "gen",
					Cmd:     []string{"go", "run", "."},
					Inputs:  []string{"a"},
					Outputs: []string{"b"},
					Tools: []spec.DeclaredTool{
						{Resolver: "go-local", Entry: "."},
					},
				}},
			},
		},
		{
			name: "command with pnpm-local tool",
			yaml: `commands:
  - name: gen
    cmd: ["pnpm", "exec", "my-codegen"]
    inputs: ["**/*.proto"]
    outputs: ["**/*.pb.ts"]
    tools:
      - pnpm-local: "@org/my-codegen"
`,
			want: &spec.File{
				Commands: []spec.Command{{
					Name:    "gen",
					Cmd:     []string{"pnpm", "exec", "my-codegen"},
					Inputs:  []string{"**/*.proto"},
					Outputs: []string{"**/*.pb.ts"},
					Tools: []spec.DeclaredTool{
						{Resolver: "pnpm-local", PackageName: "@org/my-codegen"},
					},
				}},
			},
		},
		{
			name: "command mixing script, go-local, and pnpm-local tools",
			yaml: `commands:
  - name: gen
    cmd: ["pnpm", "exec", "my-codegen"]
    inputs: ["**/*.proto"]
    outputs: ["**/*.pb.ts"]
    tools:
      - exec: ["pnpm", "--version"]
      - go-local: ./cmd/codegen
      - pnpm-local: "@org/my-codegen"
`,
			want: &spec.File{
				Commands: []spec.Command{{
					Name:    "gen",
					Cmd:     []string{"pnpm", "exec", "my-codegen"},
					Inputs:  []string{"**/*.proto"},
					Outputs: []string{"**/*.pb.ts"},
					Tools: []spec.DeclaredTool{
						{Resolver: "script", Exec: []string{"pnpm", "--version"}},
						{Resolver: "go-local", Entry: "./cmd/codegen"},
						{Resolver: "pnpm-local", PackageName: "@org/my-codegen"},
					},
				}},
			},
		},
		{
			name: "tools entry mixing exec and pnpm-local fails",
			yaml: `commands:
  - name: gen
    cmd: foo
    inputs: ["a"]
    outputs: ["b"]
    tools:
      - exec: ["foo", "--version"]
        pnpm-local: "@org/foo"
`,
			wantErr: true,
		},
		{
			name: "tools entry mixing go-local and pnpm-local fails",
			yaml: `commands:
  - name: gen
    cmd: foo
    inputs: ["a"]
    outputs: ["b"]
    tools:
      - go-local: ./cmd/foo
        pnpm-local: "@org/foo"
`,
			wantErr: true,
		},
		{
			name: "go-local parent-relative entry is accepted",
			yaml: `commands:
  - name: gen
    cmd: go run ../cmd/gen
    inputs: ["a"]
    outputs: ["b"]
    tools:
      - go-local: ../cmd/gen
`,
			want: &spec.File{
				Commands: []spec.Command{{
					Name:    "gen",
					Cmd:     []string{"go", "run", "../cmd/gen"},
					Inputs:  []string{"a"},
					Outputs: []string{"b"},
					Tools: []spec.DeclaredTool{
						{Resolver: "go-local", Entry: "../cmd/gen"},
					},
				}},
			},
		},
		{
			name: "empty commands fails",
			yaml: `commands: []
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
	mustWrite(t, filepath.Join(root, "a", "lazygen.yml"), `commands:
  - name: alpha
    cmd: do-a
    inputs: ["**/*.in"]
    outputs: ["**/*.out"]
    tools:
      - exec: ["do-a", "--version"]
`)
	mustWrite(t, filepath.Join(root, "nested", "b", "lazygen.yml"), `commands:
  - name: beta
    cmd: do-b
    inputs: ["**/*.in"]
    outputs: ["**/*.out"]
    tools:
      - exec: ["do-b", "--version"]
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

func TestDiscover_DuplicateTaskAcrossSpecsAllowed(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a", "lazygen.yml"), `commands:
  - name: gen
    cmd: foo
    inputs: ["a.in"]
    outputs: ["a.out"]
    tools:
      - exec: ["foo", "--version"]
`)
	mustWrite(t, filepath.Join(root, "b", "lazygen.yml"), `commands:
  - name: gen
    cmd: bar
    inputs: ["b.in"]
    outputs: ["b.out"]
    tools:
      - exec: ["bar", "--version"]
`)
	got, err := spec.Discover(root, "**/lazygen.yml")
	if err != nil {
		t.Fatalf("same name in different spec dirs should not error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got)=%d, want 2", len(got))
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
