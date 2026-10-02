//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "axiom-sandbox-setup is only supported on Windows")
	os.Exit(2)
}
