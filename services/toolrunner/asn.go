package toolrunner

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/dnsmsg"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
)

// CymruOrigin is a Team Cymru IP-to-ASN TXT answer ("15169 | 8.8.8.0/24 |
// US | arin | 2014-03-14"; several origin ASes are space-separated).
type CymruOrigin struct {
	ASNs     []int64
	Prefix   netip.Prefix
	Country  string
	Registry string
}

// ParseCymruOrigin parses one origin TXT string.
func ParseCymruOrigin(s string) (CymruOrigin, bool) {
	f := strings.Split(s, "|")
	if len(f) < 2 {
		return CymruOrigin{}, false
	}
	var o CymruOrigin
	for _, a := range strings.Fields(f[0]) {
		n, err := strconv.ParseInt(a, 10, 64)
		if err != nil || n < 1 || n > 1<<32-1 {
			return CymruOrigin{}, false
		}
		o.ASNs = append(o.ASNs, n)
	}
	p, err := netip.ParsePrefix(strings.TrimSpace(f[1]))
	if err != nil || len(o.ASNs) == 0 {
		return CymruOrigin{}, false
	}
	o.Prefix = p.Masked()
	if len(f) > 2 {
		o.Country = strings.TrimSpace(f[2])
	}
	if len(f) > 3 {
		o.Registry = strings.TrimSpace(f[3])
	}
	return o, true
}

// CymruName returns the origin lookup name of a (origin.asn.cymru.com for
// IPv4, origin6.asn.cymru.com for IPv6).
func CymruName(a netip.Addr) string {
	r := dnsmsg.ReverseName(a)
	if a.Unmap().Is4() {
		return strings.TrimSuffix(r, "in-addr.arpa.") + "origin.asn.cymru.com."
	}
	return strings.TrimSuffix(r, "ip6.arpa.") + "origin6.asn.cymru.com."
}

// ASN is the passive asn tool (spec 6.4 "ASN and netblock lookup",
// V2_DESIGN §8): the RDAP ip network of the address (Netblock, IPNetRecord,
// registration and contains edges, and ARIN's origin-AS extension), the
// Team Cymru origin TXT record through the proxy's DNS forwarder (the
// announced prefix and origin AS), and the RDAP autnum object of the AS.
// It emits AutonomousSystem —announces→ Netblock —contains→ IPAddress and
// returns the origin AS.
type ASN struct {
	// BootstrapURL defaults to DefaultBootstrapURL.
	BootstrapURL string
	// NoCymru disables the Team Cymru DNS lookup.
	NoCymru bool
	once    sync.Once
	c       *rdapClient
}

func (a *ASN) client() *rdapClient {
	a.once.Do(func() { a.c = &rdapClient{bootstrapURL: a.BootstrapURL} })
	return a.c
}

// Spec implements Adapter.
func (*ASN) Spec() Spec {
	return Spec{Name: "asn", Version: "1.0.0", Description: "IP to ASN and netblock (RDAP, Team Cymru DNS)",
		InputType: "IpAddress", OutputType: "Option<Asn>",
		Observes: []string{oam.TypeAutonomousSystem, oam.TypeNetblock, oam.TypeIPAddress, oam.TypeIPNetRecord, oam.TypeAutnumRecord, oam.TypeOrganization},
		Class:    core.ClassPassive, RateRPS: 2, RateBurst: 4, Concurrency: 2, Timeout: 2 * time.Minute, Idempotent: true}
}

// Run implements Adapter.
func (a *ASN) Run(ctx context.Context, input json.RawMessage, tc *ToolCtx, out Emitter) (json.RawMessage, error) {
	s, err := decodeHost(input)
	if err != nil {
		return nil, err
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || addr.Zone() != "" {
		return nil, Errorf("invalid_input", false, "input is not an IP address")
	}
	addr = addr.Unmap()
	nw := tc.Network()
	if nw == nil {
		return nil, ErrNoEgress
	}
	d, ok, err := tc.Allowed(ctx, out, addr.String(), scope.TargetIP, core.ClassPassive)
	if !ok {
		if err != nil {
			return nil, err
		}
		return json.RawMessage("null"), nil
	}
	hc := nw.IntelClient(tc, d)
	c := a.client()
	b := &obsBuilder{refs: map[string]string{}, obs: oam.Observation{ObservationID: tc.ObservationID("asn", addr.String()), ObservedAt: tc.Now(), Confidence: 0.85}}
	ipRef := b.entity("ip:"+addr.String(), oam.IPAddress{Address: addr.String(), IPVersion: ipVersion(addr)})
	orgs := map[string]oam.Organization{}
	soft := func(what string, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if te, ok := isToolError(err); ok {
			_ = out.Skipped(what, te.Code)
		} else if !errors.Is(err, errRDAPNotFound) {
			_ = out.Skipped(what, "unavailable")
		}
		return nil
	}
	// RDAP ip network.
	var alloc netip.Prefix
	var origins []int64
	kind := "ipv4"
	if addr.Is6() {
		kind = "ipv6"
	}
	if bs, err := c.bootstrap(ctx, hc, kind, tc.Now()); err != nil {
		if err := soft("rdap bootstrap "+kind, err); err != nil {
			return nil, err
		}
	} else if o, err := c.object(ctx, hc, bs.ForIP(addr), "ip/"+addr.String()); err != nil {
		if err := soft("rdap ip "+addr.String(), err); err != nil {
			return nil, err
		}
	} else if o.ObjectClassName == "ip network" {
		alloc, _ = addNetwork(b, o, addr, orgs)
		origins = o.OriginAutnums
	}
	// Team Cymru origin.
	var cy CymruOrigin
	haveCymru := false
	if !a.NoCymru {
		ans, err := nw.LookupDNS(ctx, tc, d, CymruName(addr), dnsmsg.TypeTXT)
		switch {
		case err == nil:
			for _, rr := range ans.Answers {
				if rr.Data.Kind == dnsmsg.KindTXT {
					if o, ok := ParseCymruOrigin(strings.Join(rr.Data.Strings, "")); ok && o.Prefix.Contains(addr) {
						cy, haveCymru = o, true
						break
					}
				}
			}
		case ctx.Err() != nil:
			return nil, ctx.Err()
		default:
			_ = out.Skipped(CymruName(addr), "cymru_unavailable")
		}
	}
	var asn int64
	announced := alloc
	switch {
	case haveCymru:
		asn, announced = cy.ASNs[0], cy.Prefix
	case len(origins) > 0 && origins[0] > 0 && origins[0] <= 1<<32-1:
		asn = origins[0]
	}
	if asn > 0 && announced.IsValid() {
		aref := b.entity("asn:"+strconv.FormatInt(asn, 10), oam.AutonomousSystem{Number: asn})
		nref := b.entity("net:"+announced.String(), oam.Netblock{CIDR: announced.String(), IPVersion: ipVersion(announced.Addr())})
		b.edge(aref, nref, oam.SimpleRelation{Label: oam.LabelAnnounces})
		b.edge(nref, ipRef, oam.SimpleRelation{Label: oam.LabelContains})
		if bs, err := c.bootstrap(ctx, hc, "asn", tc.Now()); err != nil {
			if err := soft("rdap bootstrap asn", err); err != nil {
				return nil, err
			}
		} else if o, err := c.object(ctx, hc, bs.ForASN(asn), "autnum/"+strconv.FormatInt(asn, 10)); err != nil {
			if err := soft("rdap autnum "+strconv.FormatInt(asn, 10), err); err != nil {
				return nil, err
			}
		} else if o.ObjectClassName == "autnum" && o.autnumNumber() == asn {
			addAutnum(b, o, orgs)
		}
	}
	if err := out.Observation(b.obs); err != nil {
		return nil, err
	}
	if asn == 0 {
		return json.RawMessage("null"), nil
	}
	return json.Marshal("AS" + strconv.FormatInt(asn, 10))
}
