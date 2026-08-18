package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"nebulous/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "neb:", err)
		os.Exit(1)
	}
}
