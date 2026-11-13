package toolrunner

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/testkit"
)

// recordedBodies returns the JSON bodies recorded for tool in
// conformance/tools.
func recordedBodies(t testing.TB, tool string) [][]byte {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(testkit.ConformanceDir(t, "tools"), tool, "*.json"))
	var out [][]byte
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var fx struct {
			HTTP []struct {
				Body json.RawMessage `json:"body"`
			} `json:"http"`
		}
		if err := json.Unmarshal(b, &fx); err != nil {
			t.Fatal(err)
		}
		for _, h := range fx.HTTP {
			if len(h.Body) > 0 {
				out = append(out, h.Body)
			}
		}
	}
	return out
}

// FuzzCTLogJSON: the crt.sh parser is total, and every name it derives is
// a normalized DNS name accepted by oam as an FQDN.
func FuzzCTLogJSON(f *testing.F) {
	for _, b := range recordedBodies(f, "ctlog") {
		f.Add(b)
	}
	f.Add([]byte(`[{"id":1,"name_value":"*.a.b\nx@y.z\n1.2.3.4\nA.B.","common_name":"..."}]`))
	f.Fuzz(func(t *testing.T, b []byte) {
		es, err := ParseCTLogJSON(b)
		if err != nil {
			return
		}
		for _, e := range es {
			for _, n := range CTNames(e) {
				nt, err := scope.Normalize(n, scope.TargetHost)
				if err != nil || !nt.IsName() || nt.Name() != n || strings.ContainsAny(n, "*@") {
					t.Fatalf("derived name %q is not normalized", n)
				}
				if _, err := (oam.FQDN{Name: n}).Key(); err != nil {
					t.Fatalf("oam rejects derived name %q: %v", n, err)
				}
			}
		}
	})
}

// FuzzRDAPJSON: the RDAP object, jCard and bootstrap parsers are total;
// mapped records, netblocks and organizations are valid oam assets, and
// derived prefixes lie inside the object's address range.
func FuzzRDAPJSON(f *testing.F) {
	for _, tool := range []string{"rdap", "asn"} {
		for _, b := range recordedBodies(f, tool) {
			f.Add(b)
		}
	}
	f.Add([]byte(`{"objectClassName":"ip network","startAddress":"10.0.0.0","endAddress":"10.0.2.255"}`))
	f.Add([]byte(`{"objectClassName":"autnum","handle":"AS1","entities":[{"roles":["registrant"],"vcardArray":["vcard",[["fn",{},"text",3],["org",{},"text","X"]]]}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		if bs, err := ParseBootstrap(b); err == nil {
			_ = bs.ForDomain("www.example.com")
			_ = bs.ForIP(netip.MustParseAddr("192.0.2.1"))
			_ = bs.ForASN(64500)
		}
		o, err := ParseRDAPObject(b)
		if err != nil {
			return
		}
		for _, org := range rdapOrgs(o.Entities) {
			if _, err := org.org.Key(); err != nil {
				t.Fatalf("organization: %v", err)
			}
			if org.role != "registrant" && org.role != "administrative" {
				t.Fatalf("role %q", org.role)
			}
		}
		ps := o.networkPrefixes()
		if s, err1 := netip.ParseAddr(o.StartAddress); err1 == nil && len(o.CIDRs) == 0 {
			if e, err2 := netip.ParseAddr(o.EndAddress); err2 == nil {
				for _, p := range ps {
					if p.Addr().Less(s) || e.Less(lastAddr(p)) {
						t.Fatalf("prefix %s outside %s-%s", p, s, e)
					}
				}
			}
		}
		for _, p := range ps {
			if _, err := (oam.Netblock{CIDR: p.String()}).Key(); err != nil {
				t.Fatalf("netblock %s: %v", p, err)
			}
		}
		if rec := o.ipNetRecord(); rec.Handle != "" || rec.CIDR != "" {
			if _, err := rec.Key(); err != nil {
				t.Fatalf("ipnet record: %v", err)
			}
		}
		b2 := &obsBuilder{refs: map[string]string{}}
		addAutnum(b2, o, map[string]oam.Organization{})
		addNetwork(b2, o, netip.MustParseAddr("192.0.2.1"), map[string]oam.Organization{})
		for _, e := range b2.obs.Entities {
			if _, err := e.Asset.Key(); err != nil {
				t.Fatalf("emitted asset %T: %v", e.Asset, err)
			}
		}
	})
}

// TestRangeCIDRs: the prefixes exactly tile the range.
func TestRangeCIDRs(t *testing.T) {
	cases := map[[2]string][]string{
		{"193.0.0.0", "193.0.7.255"}:     {"193.0.0.0/21"},
		{"10.0.0.0", "10.0.2.255"}:       {"10.0.0.0/23", "10.0.2.0/24"},
		{"10.0.0.1", "10.0.0.2"}:         {"10.0.0.1/32", "10.0.0.2/32"},
		{"2001:db8::", "2001:db8::ffff"}: {"2001:db8::/112"},
		{"0.0.0.0", "255.255.255.255"}:   {"0.0.0.0/0"},
	}
	for in, want := range cases {
		var got []string
		for _, p := range rangeCIDRs(netip.MustParseAddr(in[0]), netip.MustParseAddr(in[1])) {
			got = append(got, p.String())
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%v: %v, want %v", in, got, want)
		}
	}
	if rangeCIDRs(netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.1")) != nil {
		t.Error("reversed range")
	}
}

// TestCymru: origin parsing and lookup names.
func TestCymru(t *testing.T) {
	o, ok := ParseCymruOrigin("15169 36040 | 8.8.8.0/24 | US | arin | 2023-12-28")
	if !ok || len(o.ASNs) != 2 || o.ASNs[1] != 36040 || o.Prefix.String() != "8.8.8.0/24" || o.Registry != "arin" {
		t.Fatalf("%+v %v", o, ok)
	}
	for _, bad := range []string{"", "x | 1.2.3.0/24", "0 | 1.2.3.0/24", "15169 | nope"} {
		if _, ok := ParseCymruOrigin(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	if n := CymruName(netip.MustParseAddr("8.8.4.4")); n != "4.4.8.8.origin.asn.cymru.com." {
		t.Fatal(n)
	}
	if n := CymruName(netip.MustParseAddr("2001:db8::1")); !strings.HasSuffix(n, ".8.b.d.0.1.0.0.2.origin6.asn.cymru.com.") {
		t.Fatal(n)
	}
}
