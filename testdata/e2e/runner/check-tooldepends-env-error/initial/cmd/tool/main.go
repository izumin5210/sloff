// Self-contained generator with no dependency on generated sources: it
// resolves fine on the initial tree. The fixture removes this directory
// before check to simulate a resolution failure whose depends producers are
// all clean (an environment problem, not drift).
package main

import "os"

func main() {
	in, err := os.ReadFile("input.txt")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile("output.txt", in, 0o644); err != nil {
		panic(err)
	}
}
