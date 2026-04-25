package main

import (
	"fmt"
	"os"

	"teleport-ai/internal/gcpcli"
)

func main() {
	if err := gcpcli.Root().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
