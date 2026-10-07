// Command gcpemu is a local emulator for Google Cloud services.
package main

import (
	"fmt"
	"os"

	"github.com/linuxuser586/gcpemu/internal/cli"
	"github.com/linuxuser586/gcpemu/services"
)

func main() {
	if err := cli.New(services.Factories()).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "gcpemu:", err)
		os.Exit(1)
	}
}
