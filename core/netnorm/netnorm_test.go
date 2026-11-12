package netnorm_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/core/netnorm"
	"github.com/genuinebnt/flowforge/testkit"
)

func TestHostname(t *testing.T) {
	cases := []struct {
		in, want string
		err      error
	}{
		{" WWW.Acme.COM. ", "www.acme.com", nil},
		{"a.b.acme.com.", "a.b.acme.com", nil},
		{"bücher.example", "xn--bcher-kva.example", nil},
		{"_dmarc.acme.com", "_dmarc.acme.com", nil},
		{"a..b.com", "", netnorm.ErrEmptyLabel},
		{".acme.com", "", netnorm.ErrEmptyLabel},
		{"", "", netnorm.ErrEmpty},
		{strings.Repeat("a", 64) + ".com", "", netnorm.ErrLabelTooLong},
		{strings.Repeat("abcdefghi.", 26) + "com", "", netnorm.ErrNameTooLong},
		{"foo bar.com", "", netnorm.ErrBadCharacter},
		{"2130706433", "", netnorm.ErrAmbiguousIPv4},
		{"0x7f.1", "", netnorm.ErrAmbiguousIPv4},
		{"0177.0.0.1", "", netnorm.ErrAmbiguousIPv4},
		{"acme.com.evil.com", "acme.com.evil.com", nil},
	}
	for _, c := range cases {
		got, err := netnorm.Hostname(c.in)
		if c.err != nil {
			if !errors.Is(err, c.err) {
				t.Errorf("Hostname(%q) err = %v, want %v", c.in, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("Hostname(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestIP(t *testing.T) {
	good := map[string]string{"203.0.113.10": "203.0.113.10", "::ffff:10.0.0.1": "10.0.0.1", "2001:DB8::1": "2001:db8::1"}
	for in, want := range good {
		a, err := netnorm.IP(in)
		if err != nil || a.String() != want {
			t.Errorf("IP(%q) = %v, %v; want %s", in, a, err, want)
		}
	}
	for _, in := range []string{"2130706433", "0177.0.0.1", "0x7f.1", "127.1", "fe80::1%eth0", ""} {
		if _, err := netnorm.IP(in); err == nil {
			t.Errorf("IP(%q) accepted", in)
		}
	}
}

func TestCIDR(t *testing.T) {
	p, err := netnorm.CIDR("203.0.113.77/24")
	if err != nil || p.String() != "203.0.113.0/24" {
		t.Fatalf("got %v %v", p, err)
	}
	p, err = netnorm.CIDR("::ffff:10.0.0.0/104")
	if err != nil || p.String() != "10.0.0.0/8" {
		t.Fatalf("mapped: got %v %v", p, err)
	}
}

func TestParseURL(t *testing.T) {
	cases := map[string]string{
		"https://Admin.Corp.Acme.com:443/login":    "https://admin.corp.acme.com/login",
		"HTTP://acme.com:80/a/./b/../c#frag":       "http://acme.com/a/c",
		"https://acme.com:8443":                    "https://acme.com:8443/",
		"https://acme.com/%7Euser/%2f?q=%41%2b":    "https://acme.com/~user/%2F?q=A%2B",
		"https://[2001:db8::1]:443/x":              "https://[2001:db8::1]/x",
		"http://acme.com/a/b/..":                   "http://acme.com/a/",
		"https://api.acme.com/v2/../../../etc":     "https://api.acme.com/etc",
		"https://acme.com.evil.com/":               "https://acme.com.evil.com/",
		"https://xn--bcher-kva.example/":           "https://xn--bcher-kva.example/",
		"https://bücher.example/":                  "https://xn--bcher-kva.example/",
		"https://acme.com:443/v2/../v2/users?id=1": "https://acme.com/v2/users?id=1",
	}
	for in, want := range cases {
		u, err := netnorm.ParseURL(in)
		if err != nil || u.String() != want {
			t.Errorf("ParseURL(%q) = %q, %v; want %q", in, u.String(), err, want)
		}
	}
	bad := map[string]error{
		"https://acme.com@evil.com/": netnorm.ErrUserinfo,
		"ftp://acme.com/":            netnorm.ErrBadScheme,
		"https://2130706433/":        netnorm.ErrAmbiguousIPv4,
		"https://[fe80::1%25eth0]/":  netnorm.ErrIPZone,
		"https:///nohost":            netnorm.ErrBadURL,
		"https://acme.com:99999/":    netnorm.ErrBadURL,
		"https://user:pw@acme.com/":  netnorm.ErrUserinfo,
	}
	for in, want := range bad {
		if _, err := netnorm.ParseURL(in); !errors.Is(err, want) {
			t.Errorf("ParseURL(%q) err = %v, want %v", in, err, want)
		}
	}
}

func TestRegistrableDomain(t *testing.T) {
	if got := netnorm.RegistrableDomain("api.eu.acme.co.uk"); got != "acme.co.uk" {
		t.Fatalf("got %s", got)
	}
}

func TestNetnormProp_Idempotent(t *testing.T) {
	rng := testkit.Rng(t)
	alphabet := []byte("abcXYZ019-_.:/%@[]üé ")
	for range testkit.Cases() {
		n := 1 + int(rng.Uint64()%24)
		b := make([]byte, 0, n)
		for range n {
			b = append(b, alphabet[rng.Uint64()%uint64(len(alphabet))])
		}
		s := string(b)
		if h, err := netnorm.Hostname(s); err == nil {
			if h2, err := netnorm.Hostname(h); err != nil || h2 != h {
				t.Fatalf("Hostname not idempotent on %q: %q -> %q (%v)", s, h, h2, err)
			}
		}
		if u, err := netnorm.ParseURL("https://" + s); err == nil {
			if u2, err := netnorm.ParseURL(u.String()); err != nil || u2.String() != u.String() {
				t.Fatalf("ParseURL not idempotent on %q: %q -> %q (%v)", s, u.String(), u2.String(), err)
			}
		}
	}
}

func FuzzNetnormURL(f *testing.F) {
	for _, s := range []string{"https://acme.com/", "https://acme.com@evil.com/", "http://[::1]:80/a/../b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		u, err := netnorm.ParseURL(s)
		if err != nil {
			return
		}
		u2, err := netnorm.ParseURL(u.String())
		if err != nil || u2.String() != u.String() {
			t.Fatalf("not idempotent: %q -> %q -> %q (%v)", s, u.String(), u2.String(), err)
		}
	})
}
