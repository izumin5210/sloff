// Minimal repo-local generator nested under spec/. The lazygen.yml in spec/
// references it via `go run ./cmd/copy`, so the resolver must rebase the entry
// to repo-relative form before handing it to packages.Load.
package main

import (
	"io"
	"os"
)

func main() {
	in, err := os.Open("input.txt")
	if err != nil {
		panic(err)
	}
	defer in.Close()
	out, err := os.Create("output.txt")
	if err != nil {
		panic(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		panic(err)
	}
}
