// Command waxflow is the WaxFlow CLI and daemon entry point.
package main

import (
	"os"

	"github.com/colespringer/waxflow/cli"
)

// version is stamped by the build: -ldflags "-X main.version=v0.0.1".
var version = "dev"

func main() {
	os.Exit(cli.Execute(version, os.Args[1:], os.Stdout, os.Stderr))
}
