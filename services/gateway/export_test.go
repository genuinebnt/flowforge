package gateway

import "net/http"

// ClientIPForTest exposes clientIP to the external tests.
func ClientIPForTest(r *http.Request, hops int) string { return clientIP(r, hops) }
