package oam_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/genuinebnt/flowforge/oam"
)

func ptr[T any](v T) *T { return &v }

func sampleAssets() []oam.Asset {
	return []oam.Asset{
		oam.FQDN{Name: "api.acme.com"},
		oam.IPAddress{Address: "203.0.113.10", IPVersion: oam.IPv4},
		oam.Netblock{CIDR: "2001:db8::/32", IPVersion: oam.IPv6},
		oam.AutonomousSystem{Number: 64496},
		oam.URL{URL: "https://api.acme.com/x?y=1", Scheme: "https", Host: "api.acme.com", Port: 443, Path: "/x", Options: "y=1", Fragment: "f"},
		oam.Service{UniqueID: "abc", ServiceType: "http", Output: "HTTP/1.1 200", OutputLength: 12, Attributes: map[string][]string{"server": {"nginx"}}},
		oam.TLSCertificate{FingerprintSHA256: oam.TLSFingerprint([]byte("der")), SerialNumber: "01", KeyUsage: []string{oam.KeyUsageDigitalSignature}, IsCA: ptr(false)},
		oam.File{URL: "https://acme.com/a.pdf", Name: "a.pdf", FileType: "application/pdf"},
		oam.DomainRecord{Domain: "acme.com", Status: []string{"ok"}, DNSSEC: ptr(true)},
		oam.IPNetRecord{Handle: "NET-1", CIDR: "203.0.113.0/24", NetType: "ASSIGNED"},
		oam.AutnumRecord{Number: 64496, Handle: "AS64496"},
		oam.Organization{UniqueID: "acme", Name: "Acme", TargetMarkets: []string{"EU"}, Active: ptr(false), Headcount: 10},
		oam.Opaque{Type: oam.TypeProductRelease, Content: map[string]any{"name": "nginx", "version": "1.25.3"}},
	}
}

func TestAssetCodecRoundTrip(t *testing.T) {
	for _, a := range sampleAssets() {
		t.Run(a.AssetType(), func(t *testing.T) {
			b, err := oam.EncodeAsset(a)
			if err != nil {
				t.Fatal(err)
			}
			var head struct{ Type string }
			if err := json.Unmarshal(b, &head); err != nil || head.Type != a.AssetType() {
				t.Fatalf("discriminator missing from %s", b)
			}
			back, err := oam.DecodeAsset(b, oam.Strict)
			if err != nil {
				t.Fatalf("decode %s: %v", b, err)
			}
			if mustCanon(t, back) != mustCanon(t, a) {
				t.Fatalf("round trip: %s vs %s", mustCanon(t, back), mustCanon(t, a))
			}
			typ, content, err := oam.SplitAsset(a)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := content["type"]; ok || typ != a.AssetType() {
				t.Fatalf("split kept type: %v", content)
			}
			joined, err := oam.JoinAsset(typ, content, oam.Strict)
			if err != nil {
				t.Fatal(err)
			}
			if mustCanon(t, joined) != mustCanon(t, a) {
				t.Fatalf("join: %s", mustCanon(t, joined))
			}
			k1, err1 := a.Key()
			k2, err2 := joined.Key()
			if err1 != nil || err2 != nil || k1 != k2 {
				t.Fatalf("keys %q %q (%v, %v)", k1, k2, err1, err2)
			}
		})
	}
}

func TestOpaqueRules(t *testing.T) {
	for _, bad := range []oam.Opaque{
		{Type: oam.TypeFQDN, Content: map[string]any{"name": "a.b"}},
		{Type: "Fqdn", Content: map[string]any{"name": "a.b"}},
		{Type: "", Content: nil},
		{Type: "Person", Content: map[string]any{"type": "x"}},
		{Type: "Person", Content: map[string]any{"f": func() {}}},
	} {
		if _, err := bad.Key(); !errors.Is(err, oam.ErrInvalidAsset) {
			t.Errorf("%#v: %v", bad, err)
		}
	}
	// Go values and their JSON-decoded forms give the same key.
	a := oam.Opaque{Type: "Phone", Content: map[string]any{"e164": "+15550100", "ext": 12}}
	b, _ := oam.DecodeAsset([]byte(`{"type":"Phone","ext":12,"e164":"+15550100"}`), oam.Strict)
	ka, _ := a.Key()
	kb, _ := b.Key()
	if ka != kb || ka[:6] != "phone:" {
		t.Fatalf("keys %q %q", ka, kb)
	}
}

func TestServiceUniqueID(t *testing.T) {
	got := oam.ServiceUniqueID("ip:203.0.113.10", 443, "TCP", "HTTP")
	if want := oam.ServiceUniqueID("ip:203.0.113.10", 443, "tcp", "http"); got != want || len(got) != 64 {
		t.Fatalf("unique id %q vs %q", got, want)
	}
	k, err := oam.Service{UniqueID: got}.Key()
	if err != nil || k != "svc:"+got {
		t.Fatalf("key %q %v", k, err)
	}
}

func TestRelationAndPropertyCodec(t *testing.T) {
	rels := []oam.Relation{
		oam.SimpleRelation{Label: "domain"},
		oam.PortRelation{Label: "port", PortNumber: 8443, Protocol: "tcp"},
		oam.BasicDNSRelation{Label: "dns_record", Header: oam.DNSHeader{RRType: 5, Class: 1, TTL: 0}},
		oam.PrefDNSRelation{Label: "dns_record", Header: oam.DNSHeader{RRType: 15, Class: 1, TTL: 60}, Preference: 0},
		oam.SRVDNSRelation{Label: "dns_record", Header: oam.DNSHeader{RRType: 33, Class: 1, TTL: 60}, Priority: 1, Weight: 2, Port: 5060},
	}
	for _, r := range rels {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		back, err := oam.DecodeRelation(b, oam.Strict)
		if err != nil || !reflect.DeepEqual(back, r) {
			t.Fatalf("%s: %#v %v", b, back, err)
		}
		typ, label, disc, content, err := oam.SplitRelation(r)
		if err != nil || disc != r.Discriminator() {
			t.Fatalf("split %v", err)
		}
		j, err := oam.JoinRelation(typ, label, content, oam.Strict)
		if err != nil || !reflect.DeepEqual(j, r) {
			t.Fatalf("join %#v %v", j, err)
		}
	}
	props := []oam.Property{
		oam.SourceProperty{Name: "subfinder", Confidence: 0},
		oam.SimpleProperty{Name: "last_monitored", Value: "2026-11-12"},
		oam.DNSRecordProperty{Name: "txt", Header: oam.DNSHeader{RRType: 16, Class: 1, TTL: 300}, Data: "v=spf1 -all"},
		oam.VulnProperty{ID: "CVE-2024-1", Description: "d", Source: "nvd", Category: "c", Enum: "e", Ref: "r"},
		oam.FlowforgeProperty{Name: "rank", Value: json.Number("3"), Kind: oam.KindFact},
	}
	for _, p := range props {
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		back, err := oam.DecodeProperty(b, oam.Strict)
		if err != nil || !reflect.DeepEqual(back, p) {
			t.Fatalf("%s: %#v %v", b, back, err)
		}
		ptype, name, content, err := oam.SplitProperty(p)
		if err != nil || ptype != p.PropertyType() || name != p.PropertyName() {
			t.Fatalf("split %v", err)
		}
		if j, err := oam.JoinProperty(ptype, content, oam.Strict); err != nil || !reflect.DeepEqual(j, p) {
			t.Fatalf("join %#v %v", j, err)
		}
	}
}
