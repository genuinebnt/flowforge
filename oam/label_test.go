package oam_test

import (
	"errors"
	"testing"

	"github.com/genuinebnt/flowforge/oam"
)

// TestOAM004_LabelEndpoints checks spec 25.3 oam_004 and the spec 5.5
// table: docs-tagged labels between disallowed asset types are rejected
// with a typed error; unknown labels and verify-tagged labels are accepted.
func TestOAM004_LabelEndpoints(t *testing.T) {
	dns := oam.BasicDNSRelation{Label: "dns_record", Header: oam.DNSHeader{RRType: 1, Class: 1, TTL: 60}}
	simple := func(l string) oam.Relation { return oam.SimpleRelation{Label: l} }
	port := oam.PortRelation{Label: "port", PortNumber: 443, Protocol: "tcp"}
	cases := []struct {
		name     string
		rel      oam.Relation
		from, to string
		ok       bool
	}{
		{"dns_record to IP", dns, oam.TypeFQDN, oam.TypeIPAddress, true},
		{"dns_record CNAME", dns, oam.TypeFQDN, oam.TypeFQDN, true},
		{"dns_record from IP", dns, oam.TypeIPAddress, oam.TypeFQDN, false},
		{"ptr_record", oam.BasicDNSRelation{Label: "ptr_record", Header: dns.Header}, oam.TypeIPAddress, oam.TypeFQDN, true},
		{"ptr_record reversed", oam.BasicDNSRelation{Label: "ptr_record", Header: dns.Header}, oam.TypeFQDN, oam.TypeIPAddress, false},
		{"contains", simple("contains"), oam.TypeNetblock, oam.TypeIPAddress, true},
		{"contains from FQDN", simple("contains"), oam.TypeFQDN, oam.TypeIPAddress, false},
		{"announces", simple("announces"), oam.TypeAutonomousSystem, oam.TypeNetblock, true},
		{"registration", simple("registration"), oam.TypeNetblock, oam.TypeIPNetRecord, true},
		{"port from URL", port, oam.TypeURL, oam.TypeService, true},
		{"port from FQDN", port, oam.TypeFQDN, oam.TypeService, true},
		{"port to IP", port, oam.TypeFQDN, oam.TypeIPAddress, false},
		{"certificate", simple("certificate"), oam.TypeService, oam.TypeTLSCertificate, true},
		{"certificate from FQDN", simple("certificate"), oam.TypeFQDN, oam.TypeTLSCertificate, false},
		{"product_used release", simple("product_used"), oam.TypeService, oam.TypeProductRelease, true},
		{"product_used to Organization", simple("product_used"), oam.TypeService, oam.TypeOrganization, false},
		{"terms_of_service URL", simple("terms_of_service"), oam.TypeService, oam.TypeURL, true},
		{"domain", simple("domain"), oam.TypeURL, oam.TypeFQDN, true},
		{"ip_address", simple("ip_address"), oam.TypeURL, oam.TypeIPAddress, true},
		{"file", simple("file"), oam.TypeURL, oam.TypeFile, true},
		{"subsidiary", simple("subsidiary"), oam.TypeOrganization, oam.TypeOrganization, true},
		{"member", simple("member"), oam.TypeOrganization, oam.TypePerson, true},
		{"hq_address", simple("hq_address"), oam.TypeOrganization, oam.TypeLocation, true},
		{"website", simple("website"), oam.TypeOrganization, oam.TypeURL, true},
		{"id", simple("id"), oam.TypeOrganization, oam.TypeIdentifier, true},
		{"account", simple("account"), oam.TypeOrganization, oam.TypeAccount, true},
		{"funding_source person", simple("funding_source"), oam.TypeOrganization, oam.TypePerson, true},
		{"funding_source to FQDN", simple("funding_source"), oam.TypeOrganization, oam.TypeFQDN, false},
		{"owns anything", simple("owns"), oam.TypeOrganization, oam.TypeNetblock, true},
		{"owns from Person", simple("owns"), oam.TypePerson, oam.TypeNetblock, false},
		{"verify-tagged common_name not enforced", simple("common_name"), oam.TypeFQDN, oam.TypeFQDN, true},
		{"unknown label accepted", simple("hosted_on"), oam.TypeFQDN, oam.TypeNetblock, true},
		{"label case-insensitive", simple("Contains"), oam.TypeFQDN, oam.TypeIPAddress, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := oam.ValidateEdge(c.rel, c.from, c.to)
			if c.ok {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				return
			}
			var ee *oam.EdgeError
			if !errors.As(err, &ee) || !errors.Is(err, oam.ErrLabelEndpoints) {
				t.Fatalf("got %v, want *EdgeError", err)
			}
			if ee.FromType != c.from || ee.ToType != c.to {
				t.Fatalf("error fields %+v", ee)
			}
		})
	}
	// The observation path reports the same typed error.
	o := obs("x", "t", t0, 0.5, ent("a", oam.FQDN{Name: "a.acme.com"}), ent("b", oam.IPAddress{Address: "192.0.2.1"}))
	o.Edges = []oam.ObservedEdge{{From: "b", To: "a", Relation: dns}}
	var ee *oam.EdgeError
	if err := o.Validate(); !errors.As(err, &ee) || !errors.Is(err, oam.ErrInvalidObservation) {
		t.Fatalf("Validate: %v", err)
	}
}

func TestRelationNormalizeAndDiscriminator(t *testing.T) {
	cases := []struct {
		rel  oam.Relation
		disc string
		ok   bool
	}{
		{oam.PortRelation{Label: "port", PortNumber: 443, Protocol: "TCP"}, "tcp/443", true},
		{oam.PortRelation{Label: "port", PortNumber: 0, Protocol: "tcp"}, "", false},
		{oam.PortRelation{Label: "port", PortNumber: 53, Protocol: ""}, "", false},
		{oam.BasicDNSRelation{Label: "dns_record", Header: oam.DNSHeader{RRType: 28, Class: 1}}, "rr28", true},
		{oam.BasicDNSRelation{Label: "dns_record", Header: oam.DNSHeader{RRType: 0}}, "", false},
		{oam.SRVDNSRelation{Label: "dns_record", Header: oam.DNSHeader{RRType: 33, Class: 1}, Port: 5060}, "rr33", true},
		{oam.SRVDNSRelation{Label: "dns_record", Header: oam.DNSHeader{RRType: 33}, Port: 70000}, "", false},
		{oam.PrefDNSRelation{Label: "dns_record", Header: oam.DNSHeader{RRType: 15}, Preference: -1}, "", false},
		{oam.SimpleRelation{Label: ""}, "", false},
		{oam.SimpleRelation{Label: "bad label"}, "", false},
		{oam.SimpleRelation{Label: "Domain"}, "", true},
	}
	for _, c := range cases {
		n, err := c.rel.Normalize()
		if (err == nil) != c.ok {
			t.Errorf("%#v: err %v", c.rel, err)
			continue
		}
		if err == nil && n.Discriminator() != c.disc {
			t.Errorf("%#v: discriminator %q, want %q", c.rel, n.Discriminator(), c.disc)
		}
	}
}
