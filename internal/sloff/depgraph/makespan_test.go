package depgraph_test

import (
	"fmt"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/depgraph"
)

// unitDur models every task as one tick of work — makespan differences then
// come purely from scheduling order, which is exactly the signal the ADR-0020
// guard needs to isolate.
func unitDur(depgraph.Task) int { return 1 }

// simulateMakespan replays runner.runTasks' scheduling semantics on integer
// ticks and returns the makespan (the tick at which the last task finishes).
//
// The model mirrors the runner (ADR-0020 context): tasks are admitted to one
// of `slots` slots strictly in the given emit order (errgroup.SetLimit blocks
// the submitting loop, so a later task can never take a slot before an earlier
// one); an admitted task HOLDS its slot while waiting for its DependsOn tasks
// to finish, starts executing only once they all have, finishes at start+dur,
// and frees the slot only at finish. When a slot frees, the next not-yet-
// admitted task in emit order is admitted. This slot-held-while-waiting rule
// is why emit order approximates execution priority — the property ADR-0020
// optimises — and why a plain CPU benchmark of Build cannot see the win.
//
// Barrier tasks get no special casing: the runner still submits a goroutine
// for them, so they occupy a slot like any other task; callers model them
// with dur 0 so they complete in the same tick their dependencies finish.
//
// The function is event-driven and pure (no clocks, no goroutines), so the
// result is byte-stable under -race. It panics on inputs that indicate a
// broken test setup rather than a schedulable graph: a dependency outside the
// simulated set, a negative duration, slots < 1, or a deadlock where every
// slot is held by a waiter whose dependency was never admitted (impossible
// for topological emit orders, reachable only via hand-built orders).
func simulateMakespan(ordered []depgraph.Task, slots int, dur func(depgraph.Task) int) int {
	if slots < 1 {
		panic("simulateMakespan: slots must be >= 1")
	}
	n := len(ordered)
	if n == 0 {
		return 0
	}
	idxOf := make(map[depgraph.TaskRef]int, n)
	for i, t := range ordered {
		idxOf[t.Ref()] = i
	}

	const waiting = -1 // admitted, but execution (hence finish tick) not scheduled yet
	finishAt := make([]int, n)
	for i := range finishAt {
		finishAt[i] = waiting
	}
	finished := make([]bool, n)
	depsDone := func(i int) bool {
		for _, d := range ordered[i].DependsOn {
			j, ok := idxOf[d]
			if !ok {
				panic(fmt.Sprintf("simulateMakespan: %s depends on %s which is not in the simulated set",
					ordered[i].Ref().Label(), d.Label()))
			}
			if !finished[j] {
				return false
			}
		}
		return true
	}

	now := 0
	next := 0 // next emit-order index to admit
	running := make([]int, 0, slots)
	for done := 0; done < n; {
		for len(running) < slots && next < n {
			running = append(running, next)
			next++
		}
		// A slot holder starts executing — its finish tick becomes known — only
		// once every dependency has finished; until then it just sits on the slot.
		for _, i := range running {
			if finishAt[i] != waiting || !depsDone(i) {
				continue
			}
			d := dur(ordered[i])
			if d < 0 {
				panic("simulateMakespan: negative duration")
			}
			finishAt[i] = now + d
		}
		earliest := -1
		for _, i := range running {
			if finishAt[i] == waiting {
				continue
			}
			if earliest < 0 || finishAt[i] < earliest {
				earliest = finishAt[i]
			}
		}
		if earliest < 0 {
			panic("simulateMakespan: deadlock — every slot is held by a task waiting on a not-yet-admitted dependency")
		}
		now = earliest
		kept := running[:0]
		for _, i := range running {
			if finishAt[i] == now {
				finished[i] = true
				done++
			} else {
				kept = append(kept, i)
			}
		}
		running = kept
	}
	return now
}

func TestSimulateMakespan_IndependentTasksFillSlots(t *testing.T) {
	// 3 independent unit tasks over 2 slots: two run in [0,1), the third is
	// admitted at t=1 and runs in [1,2) → makespan 2.
	ordered := []depgraph.Task{
		taskD("", "a", nil, []string{"a.out"}),
		taskD("", "b", nil, []string{"b.out"}),
		taskD("", "c", nil, []string{"c.out"}),
	}
	if got := simulateMakespan(ordered, 2, unitDur); got != 2 {
		t.Errorf("makespan = %d, want 2", got)
	}
}

func TestSimulateMakespan_ChainOnSingleSlot(t *testing.T) {
	// A then B (B depends on A) on 1 slot: A runs in [0,1), B is admitted at
	// t=1 with its dependency already finished and runs in [1,2) → makespan 2.
	ordered := []depgraph.Task{
		taskD("", "A", nil, []string{"a.out"}),
		taskD("", "B", nil, []string{"b.out"}, ref("", "A")),
	}
	if got := simulateMakespan(ordered, 1, unitDur); got != 2 {
		t.Errorf("makespan = %d, want 2", got)
	}
}

// TestSimulateMakespan_WaiterHoldsSlotWhileBlocked locks the semantics that
// distinguish the runner from a work-conserving scheduler: a task admitted
// before its dependency keeps its slot while blocked.
//
// Hand computation (slots=2, emit order [B, A, C], B depends on A,
// dur: A=2, B=1, C=2):
//
//	t=0: B and A are admitted. B waits on A while HOLDING its slot; A
//	     executes over [0,2). C cannot be admitted — no free slot.
//	t=2: A finishes and frees a slot; C is admitted and executes over [2,4);
//	     B starts and executes over [2,3).
//	makespan = 4.
//
// A work-conserving scheduler (waiter yields its slot) would run A and C in
// parallel from t=0 and finish at t=3; asserting 4 here pins the
// slot-held-while-waiting model.
func TestSimulateMakespan_WaiterHoldsSlotWhileBlocked(t *testing.T) {
	ordered := []depgraph.Task{
		taskD("", "B", nil, []string{"b.out"}, ref("", "A")),
		taskD("", "A", nil, []string{"a.out"}),
		taskD("", "C", nil, []string{"c.out"}),
	}
	dur := func(tk depgraph.Task) int {
		if tk.Name == "B" {
			return 1
		}
		return 2
	}
	if got := simulateMakespan(ordered, 2, dur); got != 4 {
		t.Errorf("makespan = %d, want 4 (waiter must hold its slot)", got)
	}
}

func TestSimulateMakespan_BarrierFinishesInSameTick(t *testing.T) {
	// The runner completes a barrier without executing anything, so it is
	// modelled as dur 0: on 1 slot, A runs in [0,1), the barrier is admitted
	// and completes at t=1 within the same tick, B runs in [1,2) → makespan 2.
	ordered := []depgraph.Task{
		taskD("", "A", nil, []string{"a.out"}),
		barrier("", "all", ref("", "A")),
		taskD("", "B", nil, []string{"b.out"}, ref("", "all")),
	}
	dur := func(tk depgraph.Task) int {
		if tk.Barrier {
			return 0
		}
		return 1
	}
	if got := simulateMakespan(ordered, 1, dur); got != 2 {
		t.Errorf("makespan = %d, want 2", got)
	}
}

func TestSimulateMakespan_AllSlotsHeldByWaitersPanics(t *testing.T) {
	// With 1 slot and a non-topological emit order [B, A] (B depends on A),
	// B holds the only slot forever — the simulator must refuse to hang.
	defer func() {
		if recover() == nil {
			t.Error("expected panic when every slot is held by a waiter")
		}
	}()
	ordered := []depgraph.Task{
		taskD("", "B", nil, []string{"b.out"}, ref("", "A")),
		taskD("", "A", nil, []string{"a.out"}),
	}
	simulateMakespan(ordered, 1, unitDur)
}

// starvationGraph reproduces the pathology shape ADR-0020 was written for
// (a ~500-task production monorepo): `wide` shallow codegen tasks (gen:buf-NNNN,
// each a direct input of the sink → downstream height 2), `chains` deep
// toolchain chains of length `depth` (toolchain:build-cCC-dD; the head has
// height depth+1) whose tails also feed the sink, and one sink (gen:generate)
// depending on every wide task and every chain tail. The wide tasks sort
// lexicographically BEFORE every chain task ("gen" < "toolchain"), so a plain
// (SpecRelpath, Name) tie-break admits the whole shallow fan first and starves
// the chains — the ~12s starvation the ADR measured.
func starvationGraph(wide, chains, depth int) []depgraph.Task {
	if wide < 1 || chains < 1 || depth < 1 {
		panic("starvationGraph: wide, chains, and depth must be >= 1")
	}
	tasks := make([]depgraph.Task, 0, wide+chains*depth+1)
	sinkDeps := make([]depgraph.TaskRef, 0, wide+chains)
	for i := range wide {
		name := fmt.Sprintf("buf-%04d", i)
		tasks = append(tasks, depgraph.Task{
			SpecRelpath: "gen",
			Name:        name,
			Inputs:      []string{"proto/schema.proto"},
			Outputs:     []string{fmt.Sprintf("gen/%s.out", name)},
		})
		sinkDeps = append(sinkDeps, depgraph.TaskRef{SpecRelpath: "gen", Name: name})
	}
	for c := range chains {
		prevOut := "toolchain/src.in"
		var prev depgraph.TaskRef
		for d := range depth {
			name := fmt.Sprintf("build-c%02d-d%d", c, d)
			task := depgraph.Task{
				SpecRelpath: "toolchain",
				Name:        name,
				Inputs:      []string{prevOut},
				Outputs:     []string{fmt.Sprintf("toolchain/%s.out", name)},
			}
			if d > 0 {
				task.DependsOn = []depgraph.TaskRef{prev}
			}
			tasks = append(tasks, task)
			prev = task.Ref()
			prevOut = task.Outputs[0]
		}
		sinkDeps = append(sinkDeps, prev)
	}
	tasks = append(tasks, depgraph.Task{
		SpecRelpath: "gen",
		Name:        "generate",
		Outputs:     []string{"gen/generate.out"},
		DependsOn:   sinkDeps,
	})
	return tasks
}

// lexicographicTopoOrder reproduces the pre-ADR-0020 emit order: Kahn's
// algorithm with a plain (SpecRelpath, Name) ascending tie-break and no
// height bias. It exists only as the baseline side of the makespan
// comparison; production Build must beat it on the starvation shape.
func lexicographicTopoOrder(tb testing.TB, tasks []depgraph.Task) []depgraph.Task {
	tb.Helper()
	idxOf := make(map[depgraph.TaskRef]int, len(tasks))
	for i, t := range tasks {
		idxOf[t.Ref()] = i
	}
	inDegree := make([]int, len(tasks))
	consumers := make([][]int, len(tasks))
	for i, t := range tasks {
		for _, d := range t.DependsOn {
			j, ok := idxOf[d]
			if !ok {
				tb.Fatalf("lexicographicTopoOrder: %s depends on unknown task %s", t.Ref().Label(), d.Label())
			}
			inDegree[i]++
			consumers[j] = append(consumers[j], i)
		}
	}
	var ready []int
	for i := range tasks {
		if inDegree[i] == 0 {
			ready = append(ready, i)
		}
	}
	out := make([]depgraph.Task, 0, len(tasks))
	for len(ready) > 0 {
		sort.Slice(ready, func(a, b int) bool {
			ra, rb := tasks[ready[a]].Ref(), tasks[ready[b]].Ref()
			if ra.SpecRelpath != rb.SpecRelpath {
				return ra.SpecRelpath < rb.SpecRelpath
			}
			return ra.Name < rb.Name
		})
		next := ready[0]
		ready = ready[1:]
		out = append(out, tasks[next])
		for _, c := range consumers[next] {
			inDegree[c]--
			if inDegree[c] == 0 {
				ready = append(ready, c)
			}
		}
	}
	if len(out) != len(tasks) {
		tb.Fatalf("lexicographicTopoOrder: cycle in task set (%d of %d emitted)", len(out), len(tasks))
	}
	return out
}

// TestBuildOrderBeatsLexicographicMakespan is the ADR-0020 regression guard.
// On the starvation-shaped graph, Build's downstream-height tie-break must
// yield a strictly smaller simulated makespan than the pre-ADR-0020 plain
// (SpecRelpath, Name) topological order: lexicographically the wide gen:buf-*
// fan sorts before every toolchain:build-* chain task, so the old order fills
// all slots with shallow tasks and starves the deep chains. This test FAILS
// if someone reverts the height tie-break in sortByPriority — that is its
// purpose.
func TestBuildOrderBeatsLexicographicMakespan(t *testing.T) {
	tasks := starvationGraph(400, 4, 5)
	ordered, err := depgraph.Build(tasks)
	if err != nil {
		t.Fatal(err)
	}
	const slots = 14 // the slot count observed in the ADR-0020 pathology (NumCPU)
	buildTicks := simulateMakespan(ordered, slots, unitDur)
	lexTicks := simulateMakespan(lexicographicTopoOrder(t, tasks), slots, unitDur)
	t.Logf("makespan: Build order = %d ticks, lexicographic order = %d ticks", buildTicks, lexTicks)
	if buildTicks >= lexTicks {
		t.Errorf("Build order makespan (%d ticks) must beat the pre-ADR-0020 lexicographic order (%d ticks); "+
			"the downstream-height tie-break of sortByPriority appears to be regressed", buildTicks, lexTicks)
	}
}

// TestBuildOrderDeterministicOnStarvationGraph guards ADR-0020 D5 at scale: the
// same task set must yield a byte-identical emit order across repeated Build
// calls even on the ~500-task pathology graph.
func TestBuildOrderDeterministicOnStarvationGraph(t *testing.T) {
	build := func() []string {
		got, err := depgraph.Build(starvationGraph(400, 4, 5))
		if err != nil {
			t.Fatal(err)
		}
		return names(got)
	}
	first := build()
	if diff := cmp.Diff(first, build()); diff != "" {
		t.Errorf("Build emit order must be deterministic (-first +second):\n%s", diff)
	}
}
