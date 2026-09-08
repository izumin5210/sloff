#!/usr/bin/env bash
# Runs one round of the sloff benchmark suite and appends the raw
# `go test -bench` output to a file. The CI bench job calls this alternately
# against the PR head and its merge-base worktree so both sides sample the
# same runner interleaved in time; benchgate then compares the two files.
# See docs/adr/0021-benchmark-suite-and-regression-gate.md.
#
# Usage: scripts/bench.sh <micro|macro> <outfile> [source-dir] [count]
#
# Never run benchmarks with -race: the race detector distorts timing. The CI
# test job keeps -race for correctness; this script is the perf path.
set -euo pipefail

mode=${1:?usage: bench.sh <micro|macro> <outfile> [source-dir] [count]}
outfile=${2:?usage: bench.sh <micro|macro> <outfile> [source-dir] [count]}
dir=${3:-.}
count=${4:-1}

# Resolve the output path before cd'ing into the target tree.
case "$outfile" in
/*) ;;
*) outfile="$(pwd)/$outfile" ;;
esac

cd "$dir"

case "$mode" in
micro)
  # Micro-benchmarks: hot pure/near-pure functions. 300ms per benchmark keeps
  # a full round short while still averaging thousands of iterations for the
  # fast paths.
  go test -run '^$' -bench . -benchtime 300ms -count "$count" -timeout 20m \
    ./internal/sloff/depgraph/... \
    ./internal/sloff/hash/... \
    ./internal/sloff/glob/... \
    ./internal/sloff/spec/... \
    ./internal/sloff/toolresolver/... \
    >>"$outfile"
  ;;
macro)
  # Macro-benchmarks: full in-process runs over the synthetic ~500-task repo.
  # One run per iteration (-benchtime=1x); passing count>1 amortises the
  # fixture generation across samples within one invocation.
  go test -run '^$' -bench '^BenchmarkRun$' -benchtime 1x -count "$count" -timeout 30m \
    ./internal/sloff/runner/ \
    >>"$outfile"
  ;;
*)
  echo "bench.sh: unknown mode '$mode' (want micro or macro)" >&2
  exit 2
  ;;
esac
