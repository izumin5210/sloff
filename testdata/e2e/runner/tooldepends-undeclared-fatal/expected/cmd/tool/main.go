// Generator that imports a generated package — the ADR-0019 bootstrap shape.
// Its import closure includes example.test/bootstrapfixture/gen, whose only
// file is produced by the gen-source task, so go-local resolution fails on a
// clean tree until that task has run.
package main

import (
	"os"

	"example.test/bootstrapfixture/gen"
)

func main() {
	in, err := os.ReadFile("input.txt")
	if err != nil {
		panic(err)
	}
	out := string(in) + gen.Suffix + "\n"
	if err := os.WriteFile("output.txt", []byte(out), 0o644); err != nil {
		panic(err)
	}
}
