//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "axiom-command-runner is only supported on Windows")
	os.Exit(2)
}
