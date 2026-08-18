package main

import (
	"context"
	"testing"
)

func TestInternalModesRejectInvalidArguments(t *testing.T) {
	for _, args := range [][]string{{"__platform"}, {"__query-exec", "extra"}} {
		if err := run(context.Background(), args); err == nil {
			t.Fatalf("internal mode accepted %#v", args)
		}
	}
}
