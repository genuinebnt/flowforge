// Command egress-proxy runs the Flowforge egress-proxy service (spec 3.1); configuration
// comes from the environment (spec 17.5, services/README.md).
package main

import "github.com/genuinebnt/flowforge/services/app"

// version is stamped at link time by deploy/Dockerfile
// (-ldflags "-X main.version=..."); nothing reports it yet, the blank
// use in main keeps the linker from dropping it.
var version = "dev"

func main() {
	_ = version
	app.Main("egress-proxy")
}
