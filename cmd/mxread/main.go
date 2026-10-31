// Command mxread is a read-only Matrix MCP server and digest CLI.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("mxread", version)
		return
	}
	fmt.Fprintln(os.Stderr, "usage: mxread serve|digest|check|version")
	os.Exit(2)
}
