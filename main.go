package main

import (
	"fmt"
	"os"

	"github.com/hoophq/blueprint/internal/cli"
)

func main() {
	if err := cli.Root().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
