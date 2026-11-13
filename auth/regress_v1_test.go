package auth_test

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/testkit"
)

// TestRegress_G_LIM_11_IPKeysAgree: auth.IPThrottleKey (login) did not
// escape ':' while limiter.AuthIPKey (signup, password reset) did, so the
// throttle keys of one IPv6 client spelled its address two ways, and
// neither normalized it, so ::ffff:192.0.2.1 and 192.0.2.1 (or two
// spellings of one IPv6 address) were different clients. Both now build
// the same canonical, escaped key.
func TestRegress_G_LIM_11_IPKeysAgree(t *testing.T) {
	classes := [][]string{
		{"192.0.2.1", "::ffff:192.0.2.1", "::FFFF:c000:0201", " 192.0.2.1 ", "[::ffff:192.0.2.1]"},
		{"2001:db8::1", "2001:DB8:0:0:0:0:0:1", "2001:0db8::0001", "[2001:db8::1]"},
		{"fe80::1%eth0", "FE80:0::1%eth0"},
		{"unknown"},
	}
	want := map[string]string{
		"192.0.2.1":    "rl:auth:ip:192.0.2.1:login",
		"2001:db8::1":  "rl:auth:ip:2001%3Adb8%3A%3A1:login",
		"fe80::1%eth0": "rl:auth:ip:fe80%3A%3A1%25eth0:login",
		"unknown":      "rl:auth:ip:unknown:login",
	}
	seen := map[string]string{}
	for _, class := range classes {
		key := want[class[0]]
		for _, ip := range class {
			for _, ep := range []string{"login", "signup", "password_forgot"} {
				a, l := auth.IPThrottleKey(ip, ep), limiter.AuthIPKey(ip, ep)
				if a != l {
					t.Errorf("%q %s: auth %q, limiter %q", ip, ep, a, l)
				}
			}
			if got := auth.IPThrottleKey(ip, "login"); got != key {
				t.Errorf("IPThrottleKey(%q) = %q, want %q", ip, got, key)
			}
		}
		if prev, dup := seen[key]; dup {
			t.Errorf("%q and %q share key %q", prev, class[0], key)
		}
		seen[key] = class[0]
	}
	// Random addresses and endpoints, IPv6 in several spellings.
	rng := testkit.Rng(t)
	for range testkit.Cases() {
		var b [16]byte
		for i := range b {
			b[i] = byte(rng.Uint64())
		}
		a := netip.AddrFrom16(b)
		forms := []string{a.String(), a.StringExpanded(), strings.ToUpper(a.StringExpanded()), "[" + a.String() + "]"}
		ep := []string{"login", "a:b", "x%y"}[rng.Uint64()%3]
		k := limiter.AuthIPKey(forms[0], ep)
		for _, f := range forms {
			if got := auth.IPThrottleKey(f, ep); got != k {
				t.Fatalf("%q %q: auth %q, limiter %q", f, ep, got, k)
			}
		}
	}
}
