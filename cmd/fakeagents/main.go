// Command fakeagents runs the OpenAI and Anthropic API simulator.
package main

import (
	"os"

	"github.com/jahrulnr/fakeagents/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
