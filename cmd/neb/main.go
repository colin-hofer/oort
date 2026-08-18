package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"nebulous/internal/cli"
	"nebulous/internal/platform"
	"nebulous/internal/queryexec"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "neb:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "__platform" {
		return platform.Run(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "__query-exec" {
		if len(args) != 1 {
			return fmt.Errorf("internal query process takes no arguments")
		}
		return queryexec.Child(os.Stdin, os.Stdout)
	}
	return cli.Run(ctx, args, os.Stdout, os.Stderr)
}
