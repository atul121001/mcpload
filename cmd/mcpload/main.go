// Command mcpload runs MCP load/soak scenarios with k6 (xk6-mcpload), samples
// server resources, computes leak/drift verdicts and writes report.json/.html.
package main

import (
	"os"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/cli"
)

// version is set at build time: -ldflags "-X main.version=v0.3.0".
var version = ""

func main() {
	if version != "" {
		cli.Version = version
	}
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
