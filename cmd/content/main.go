// Command content runs the Flowforge content service (V2 design §6-§8);
// configuration comes from the environment (services/README.md).
package main

import "github.com/genuinebnt/flowforge/services/app"

var version = "dev"

func main() {
	_ = version
	app.Main("content")
}
