// amp-cli is a test harness for WSO2 Agent Manager agents, designed to run
// inside the Kubernetes cluster where obs-gateway and AMP API are reachable
// via in-cluster DNS. It is the Go equivalent of the Python amp-instrument
// command-line wrapper, with additional subcommands for trace validation
// and connectivity health checks.
package main

import (
	"fmt"
	"os"

	"github.com/wso2/amp-go/cmd/amp-cli/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
