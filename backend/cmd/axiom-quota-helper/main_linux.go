//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"axiom.local/agent/internal/projectquota"
)

func main() {
	socketPath, workspaceRoot, allowedUID, allowedGID, maximumBytes, err := projectquota.QuotaHelperConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := projectquota.ServeQuotaHelper(ctx, socketPath, workspaceRoot, allowedUID, allowedGID, maximumBytes); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
