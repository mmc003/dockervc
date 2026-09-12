// Command dockervc provides local version control and backup for a Docker engine.
package main

import (
	"os"

	"dockervc/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		os.Exit(1)
	}
}
