// Minimal repo-local generator used by the sloff golocal e2e fixtures.
// It copies input.txt to output.txt; any code change invalidates the cache.
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
