// Command gcpemu is a local emulator for Google Cloud services.
package main

import (
	"fmt"
	"os"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/cli"
	"github.com/linuxuser586/gcpemu/services"
)

func main() {
	agent.Main() // runs an in-container agent and exits when GCPEMU_AGENT is set
	if err := cli.New(services.Factories()).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "gcpemu:", err)
		os.Exit(1)
	}
}
