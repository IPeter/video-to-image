// Command v2i is the single backend binary: serve, work, migrate, eval (and openapi).
// One container image; the Kubernetes Deployment picks the subcommand via args.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "v2i:", err)
		os.Exit(1)
	}
}
