package main

import (
	"fmt"
	"os"

	"github.com/cagojeiger/ShiftPV/src/lifecycle/cleanup/pathcheck"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: shiftpv-cleanup-check <pool-directory>")
		os.Exit(2)
	}
	if err := pathcheck.Check(os.Args[1], os.Getenv("MOVE_NAME"), os.Getenv("VOLUME_ID")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
