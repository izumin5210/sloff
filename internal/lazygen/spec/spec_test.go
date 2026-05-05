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
`,
			want: &spec.File{
				Commands: []spec.Command{{
					Name:    "protoc-gen-go",
					Cmd:     []string{"buf", "generate", "--template", "buf.gen.yaml"},
					Inputs:  []string{"**/*.proto", "buf.gen.yaml"},
					Outputs: []string{"**/*.pb.go", "**/*.connect.go"},
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
`,
			want: &spec.File{
				Commands: []spec.Command{{
					Name:    "gen",
					Cmd:     []string{"foo", "bar baz"},
					Inputs:  []string{"a"},
					Outputs: []string{"b"},
				}},
			},
		},
		{
			name: "command with tools",
			yaml: `commands:
  - name: gen
    cmd: foo
    inputs: ["a"]
    outputs: ["b"]
    tools:
      - aqua: bufbuild/buf
      - go-external: google.golang.org/protobuf/cmd/protoc-gen-go
`,
			want: &spec.File{
				Commands: []spec.Command{{
					Name:    "gen",
					Cmd:     []string{"foo"},
					Inputs:  []string{"a"},
					Outputs: []string{"b"},
					Tools: []spec.DeclaredTool{
						{Resolver: "aqua", Key: "bufbuild/buf"},
						{Resolver: "go-external", Key: "google.golang.org/protobuf/cmd/protoc-gen-go"},
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
			name: "tools entry with multiple keys fails",
			yaml: `commands:
  - name: gen
    cmd: foo
    inputs: ["a"]
    outputs: ["b"]
    tools:
      - aqua: x
        go-external: y
`,
			wantErr: true,
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
`)
	mustWrite(t, filepath.Join(root, "nested", "b", "lazygen.yml"), `commands:
  - name: beta
    cmd: do-b
    inputs: ["**/*.in"]
    outputs: ["**/*.out"]
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
`)
	mustWrite(t, filepath.Join(root, "b", "lazygen.yml"), `commands:
  - name: gen
    cmd: bar
    inputs: ["b.in"]
    outputs: ["b.out"]
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
