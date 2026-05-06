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
	if _, err := out.WriteString("v2\n"); err != nil {
		panic(err)
	}
}
