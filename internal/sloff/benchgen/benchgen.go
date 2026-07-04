// Package benchgen generates a synthetic sloff monorepo for macro-benchmarks.
//
// The generated shape mirrors the production pathology ADR-0020 was written
// against (the "layerone" deployment): a wide fan of shallow independent
// codegen tasks, several deep dependency chains, and a single sink task that
// depends on all of them. Generator commands are trivial (`cat` into the
// output file) so a benchmark over the repo measures sloff's own
// orchestration — discovery, resolution, hashing, scheduling, fingerprinting —
// rather than generator runtime, which is an explicit non-goal to optimise.
//
// Generation is deterministic for a given Params (including Seed), so two
// benchmark processes see byte-identical repos.
package benchgen

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sync/errgroup"
)

// Params configures the synthetic monorepo shape.
type Params struct {
	// Seed varies file contents (and therefore every input digest) without
	// changing the repo shape.
	Seed int64
	// WideTasks is the number of shallow, independent tasks (the `buf-*`
	// analogue: height 2, all feeding the sink).
	WideTasks int
	// Chains is the number of deep dependency chains; ChainDepth is the
	// number of tasks in each chain. Task k of a chain consumes task k-1's
	// output, so a chain is a strict sequential critical path.
	Chains     int
	ChainDepth int
	// FilesPerTask source files are generated per task, each FileSizeBytes
	// long. Total input files ≈ (WideTasks + Chains*ChainDepth) * FilesPerTask.
	FilesPerTask  int
	FileSizeBytes int
}

// DefaultParams is the scale the macro-benchmarks run at: ~500 tasks and
// ~30k input files, matching the deployment scale cited by ADR-0014/0020.
func DefaultParams() Params {
	return Params{
		Seed:          1,
		WideTasks:     400,
		Chains:        20,
		ChainDepth:    5,
		FilesPerTask:  60,
		FileSizeBytes: 256,
	}
}

// Repo describes what Generate wrote, so callers (and the generator's own
// tests) can validate the shape instead of trusting it blindly.
type Repo struct {
	Root string
	// TaskCount includes the sink task.
	TaskCount int
	// SpecFileCount counts generated sloff.yml files (one per task dir plus
	// the root tools-only spec).
	SpecFileCount int
	// SourceFileCount counts generated *.src input files.
	SourceFileCount int
	// OutputPaths lists every declared output (repo-root relative,
	// slash-separated), useful for wiping generated outputs between cold runs.
	OutputPaths []string
	// MutableInput is a chain-head source file (repo-root relative) whose
	// rewrite invalidates exactly one chain: the head re-runs, its changed
	// output cascades down the chain and into the sink, while every other
	// task stays a hit — the canonical warm-incremental scenario
	// (ChainDepth+1 tasks re-run, the rest hit).
	MutableInput string
}

// Validate rejects shapes that would silently produce a degenerate repo.
func (p Params) Validate() error {
	if p.WideTasks < 1 || p.Chains < 1 || p.ChainDepth < 2 || p.FilesPerTask < 1 || p.FileSizeBytes < 1 {
		return fmt.Errorf("benchgen: degenerate params %+v (need WideTasks/Chains/FilesPerTask/FileSizeBytes >= 1, ChainDepth >= 2)", p)
	}
	return nil
}

// Generate writes the synthetic repo under root (which must exist and be
// empty or absent). Directory layout:
//
//	sloff.yml                     — shared script tool (`vers`) only
//	gen/w0000/{sloff.yml,src/}    — wide tasks, one spec dir each
//	toolchain/c00/d0..dN/…        — chains; d(k) depends on d(k-1)
//	zz-sink/sloff.yml             — sink depending on every wide task and
//	                                every chain tail (the `generate` analogue)
//
// Spec dir names are chosen so the lexicographic (SpecRelpath, Name) order
// puts chain heads after the wide fan — the starvation shape ADR-0020 fixes.
func Generate(root string, p Params) (*Repo, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}

	repo := &Repo{Root: root}

	// Root spec declares the single shared tool. `echo` resolves in one
	// near-instant spawn per run; every command must reference at least one
	// tool (spec validation), and sharing one keeps resolve-phase cost O(1).
	rootSpec := "tools:\n  vers:\n    exec: [\"echo\", \"v1.0.0\"]\n"
	if err := os.WriteFile(filepath.Join(root, "sloff.yml"), []byte(rootSpec), 0o644); err != nil {
		return nil, err
	}
	repo.SpecFileCount++

	type taskDir struct {
		dir  string // repo-relative, slash form
		spec string // sloff.yml contents
	}
	var dirs []taskDir

	var sinkDepends []string
	var sinkInputs []string
	var sinkCat []string

	for w := range p.WideTasks {
		dir := fmt.Sprintf("gen/w%04d", w)
		dirs = append(dirs, taskDir{dir: dir, spec: taskSpecYAML("")})
		repo.OutputPaths = append(repo.OutputPaths, dir+"/out.gen")
		sinkDepends = append(sinkDepends, fmt.Sprintf("      - {spec: ../%s, task: gen}", dir))
		sinkInputs = append(sinkInputs, fmt.Sprintf("../%s/out.gen", dir))
		sinkCat = append(sinkCat, fmt.Sprintf("../%s/out.gen", dir))
		repo.TaskCount++
	}

	for c := range p.Chains {
		for d := range p.ChainDepth {
			dir := fmt.Sprintf("toolchain/c%02d/d%d", c, d)
			var upstream string
			if d > 0 {
				upstream = fmt.Sprintf("../d%d", d-1)
			}
			dirs = append(dirs, taskDir{dir: dir, spec: taskSpecYAML(upstream)})
			repo.OutputPaths = append(repo.OutputPaths, dir+"/out.gen")
			repo.TaskCount++
			if d == p.ChainDepth-1 {
				sinkDepends = append(sinkDepends, fmt.Sprintf("      - {spec: ../%s, task: gen}", dir))
				sinkInputs = append(sinkInputs, fmt.Sprintf("../%s/out.gen", dir))
				sinkCat = append(sinkCat, fmt.Sprintf("../%s/out.gen", dir))
			}
		}
	}
	repo.MutableInput = "toolchain/c00/d0/src/f0000.src"

	// Sink: consumes every wide output and every chain tail. Inputs are the
	// concrete producer outputs, and each producer edge is declared, so the
	// overlap validation (ADR-0013) passes at this scale like production does.
	sinkSpec := fmt.Sprintf(`commands:
  - name: generate
    cmd: ["sh", "-c", "cat %s > out.gen"]
    inputs:
%s
    outputs: ["out.gen"]
    tools: [vers]
    depends:
%s
`,
		strings.Join(sinkCat, " "),
		yamlStringList(sinkInputs, "      - "),
		strings.Join(sinkDepends, "\n"))
	dirs = append(dirs, taskDir{dir: "zz-sink", spec: sinkSpec})
	repo.OutputPaths = append(repo.OutputPaths, "zz-sink/out.gen")
	repo.TaskCount++

	// Write every task dir concurrently: 30k small files are cheap
	// individually but the syscalls add up; generation stays outside any
	// benchmark's timed region regardless.
	g := new(errgroup.Group)
	g.SetLimit(max(runtime.GOMAXPROCS(0), 1))
	for _, td := range dirs {
		g.Go(func() error {
			abs := filepath.Join(root, filepath.FromSlash(td.dir))
			if err := os.MkdirAll(filepath.Join(abs, "src"), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(abs, "sloff.yml"), []byte(td.spec), 0o644); err != nil {
				return err
			}
			if td.dir == "zz-sink" {
				return nil
			}
			for f := range p.FilesPerTask {
				rel := fmt.Sprintf("%s/src/f%04d.src", td.dir, f)
				if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), fileContent(p.Seed, rel, p.FileSizeBytes), 0o644); err != nil {
					return err
				}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	repo.SpecFileCount += len(dirs)
	repo.SourceFileCount = (p.WideTasks + p.Chains*p.ChainDepth) * p.FilesPerTask
	return repo, nil
}

// taskSpecYAML renders a single-task sloff.yml. upstream, when non-empty, is
// the spec-dir-relative path of the producer whose out.gen this task consumes
// (chain link); the depends edge is declared alongside the input so the
// overlap validation holds.
func taskSpecYAML(upstream string) string {
	inputs := `["src/*.src"]`
	cat := "src/*.src"
	depends := ""
	if upstream != "" {
		inputs = fmt.Sprintf(`["src/*.src", "%s/out.gen"]`, upstream)
		cat = fmt.Sprintf("src/*.src %s/out.gen", upstream)
		depends = fmt.Sprintf("\n    depends:\n      - {spec: %s, task: gen}", upstream)
	}
	return fmt.Sprintf(`commands:
  - name: gen
    cmd: ["sh", "-c", "cat %s > out.gen"]
    inputs: %s
    outputs: ["out.gen"]
    tools: [vers]%s
`, cat, inputs, depends)
}

func yamlStringList(items []string, prefix string) string {
	lines := make([]string, len(items))
	for i, it := range items {
		lines[i] = fmt.Sprintf("%s%q", prefix, it)
	}
	return strings.Join(lines, "\n")
}

// fileContent derives deterministic per-file bytes from (seed, path): a
// header that makes every file unique followed by xorshift padding, so
// digests differ across files and across seeds without any global RNG state.
func fileContent(seed int64, rel string, size int) []byte {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d\x00%s", seed, rel)
	state := h.Sum64() | 1

	buf := make([]byte, 0, size+64)
	buf = fmt.Appendf(buf, "%s seed=%d\n", rel, seed)
	for len(buf) < size {
		// xorshift64: cheap, deterministic, seedable filler.
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		buf = fmt.Appendf(buf, "%016x\n", state)
	}
	return buf[:size]
}
