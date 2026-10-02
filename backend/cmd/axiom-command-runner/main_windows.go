//go:build windows

package main

import (
	"os"

	"axiom.local/agent/internal/sandbox"
)

func main() {
	os.Exit(sandbox.RunCommandRunner(os.Args[1:]))
}
