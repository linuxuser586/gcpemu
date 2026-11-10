// Command devtool holds the control flow of the justfile's recipes (ADR
// 0061), so each recipe is one plain command per line that runs the same
// in sh and in Windows PowerShell. Run it from the repository's root:
//
//	go run ./internal/devtool <cmd> [args]
//
// It uses only the standard library, so it adds nothing to go.mod.
package main

import (
	"errors"
	"fmt"
	"os"
)

// errReported fails a command that has already said why.
var errReported = errors.New("")

var commands = map[string]func(args []string) error{
	"adr":              runADR,
	"docker-arch":      runDockerArch,
	"doctor":           runDoctor,
	"echo-image-check": runEchoImageCheck,
	"image-check":      runImageCheck,
	"need-cgo":         runNeedCgo,
	"vet":              runVet,
	"version":          runVersion,
	"which":            runWhich,
}

func main() {
	if len(os.Args) < 2 || commands[os.Args[1]] == nil {
		fmt.Fprintln(os.Stderr, "usage: go run ./internal/devtool <cmd> [args]")
		os.Exit(2)
	}
	if err := commands[os.Args[1]](os.Args[2:]); err != nil {
		if err != errReported {
			fmt.Fprintf(os.Stderr, "%s: %v\n", os.Args[1], err)
		}
		os.Exit(1)
	}
}
