package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	var (
		basePath  = flag.String("base", "", "bench output of the merge-base (old)")
		headPath  = flag.String("head", "", "bench output of the PR head (new)")
		threshold = flag.Float64("threshold", 0.30, "relative regression threshold for timing metrics (0.30 = +30%)")
		alpha     = flag.Float64("alpha", 0.05, "significance level for the timing comparison")
		minCount  = flag.Int("min-count", 4, "minimum samples per side required to gate a timing metric")
	)
	flag.Parse()
	if *basePath == "" || *headPath == "" {
		fmt.Fprintln(os.Stderr, "usage: benchgate -base old.txt -head new.txt [-threshold 0.30] [-alpha 0.05] [-min-count 4]")
		os.Exit(2)
	}

	res, err := runGate(*basePath, *headPath, gateConfig{threshold: *threshold, alpha: *alpha, minCount: *minCount})
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchgate: %v\n", err)
		os.Exit(2)
	}
	res.render(os.Stdout)
	if res.failed() {
		fmt.Fprintln(os.Stderr, "benchgate: benchmark regression detected (see table above; rebaseline guidance in docs/adr/0021-benchmark-suite-and-regression-gate.md)")
		os.Exit(1)
	}
	fmt.Println("benchgate: no gated regressions")
}
