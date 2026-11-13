// Command egress-proxy runs the Flowforge egress-proxy service (spec 3.1;
// V2_DESIGN §9); configuration comes from the environment (spec 17.5,
// services/README.md; the FF_EGRESS_* variables of
// services/egressproxy/README.md enable SOCKS5, the DNS forwarder, the
// intel allowlist and the port policy).
package main

import "github.com/genuinebnt/flowforge/services/app"

var version = "dev"

func main() {
	_ = version
	app.Main("egress-proxy")
}
