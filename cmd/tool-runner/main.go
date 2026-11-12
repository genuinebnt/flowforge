// Command tool-runner runs the Flowforge tool-runner service (spec 3.1); configuration
// comes from the environment (spec 17.5, services/README.md).
package main

import "github.com/genuinebnt/flowforge/services/app"

var version = "dev"

func main() {
	_ = version
	app.Main("tool-runner")
}
