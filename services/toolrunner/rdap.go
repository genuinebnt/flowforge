package toolrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/core/netnorm"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
)

// RDAP JSON shapes (RFC 9083) and bootstrap registries (RFC 9224), decoded
// into concrete structs. Unknown members are ignored; members of the wrong
// JSON type make the whole object invalid (the tool reports
// invalid_response).

// RDAPEvent is an events[] member.
type RDAPEvent struct {
	Action string `json:"eventAction"`
	Date   string `json:"eventDate"`
}

// RDAPEntity is an entities[] member with its jCard (RFC 7095).
type RDAPEntity struct {
	ObjectClassName string          `json:"objectClassName"`
	Handle          string          `json:"handle"`
	Roles           []string        `json:"roles"`
	VCard           json.RawMessage `json:"vcardArray"`
	Entities        []RDAPEntity    `json:"entities"`
}

// RDAPCIDR is a cidr0_cidrs member (the cidr0 extension).
type RDAPCIDR struct {
	V4Prefix string `json:"v4prefix"`
	V6Prefix string `json:"v6prefix"`
	Length   int    `json:"length"`
}

// RDAPObject is a domain, ip network or autnum object (RFC 9083 §5).
type RDAPObject struct {
	ObjectClassName string       `json:"objectClassName"`
	Handle          string       `json:"handle"`
	LDHName         string       `json:"ldhName"`
	UnicodeName     string       `json:"unicodeName"`
	Status          []string     `json:"status"`
	Port43          string       `json:"port43"`
	Events          []RDAPEvent  `json:"events"`
	Entities        []RDAPEntity `json:"entities"`
	SecureDNS       *struct {
		DelegationSigned *bool `json:"delegationSigned"`
	} `json:"secureDNS"`
	StartAddress  string     `json:"startAddress"`
	EndAddress    string     `json:"endAddress"`
	IPVersion     string     `json:"ipVersion"`
	Name          string     `json:"name"`
	Type          string     `json:"type"`
	Country       string     `json:"country"`
	ParentHandle  string     `json:"parentHandle"`
	CIDRs         []RDAPCIDR `json:"cidr0_cidrs"`
	StartAutnum   *int64     `json:"startAutnum"`
	EndAutnum     *int64     `json:"endAutnum"`
	OriginAutnums []int64    `json:"arin_originas0_originautnums"`
	ErrorCode     int        `json:"errorCode"`
}

// MaxRDAPBody bounds an RDAP or bootstrap response.
const MaxRDAPBody = 4 << 20

// ParseRDAPObject decodes one RDAP object response.
func ParseRDAPObject(b []byte) (RDAPObject, error) {
	var o RDAPObject
	if err := json.Unmarshal(b, &o); err != nil {
		return RDAPObject{}, fmt.Errorf("rdap: %w", err)
	}
	switch o.ObjectClassName {
	case "domain", "ip network", "autnum":
		return o, nil
	}
	return RDAPObject{}, fmt.Errorf("rdap: unexpected objectClassName %q", o.ObjectClassName)
}

// RDAPContact is the part of a jCard the tools use.
type RDAPContact struct{ Kind, FN, Org string }

// ParseVCard extracts kind, fn and org from a jCard ["vcard", [[name,
// params, type, value], ...]]. Properties with non-string values are
// skipped; a malformed card yields an empty contact.
func ParseVCard(raw json.RawMessage) RDAPContact {
	var outer []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &outer) != nil || len(outer) != 2 {
		return RDAPContact{}
	}
	var tag string
	if json.Unmarshal(outer[0], &tag) != nil || tag != "vcard" {
		return RDAPContact{}
	}
	var props []json.RawMessage
	if json.Unmarshal(outer[1], &props) != nil {
		return RDAPContact{}
	}
	var c RDAPContact
	for _, p := range props {
		var parts []json.RawMessage
		if json.Unmarshal(p, &parts) != nil || len(parts) < 4 {
			continue
		}
		var name, value string
		if json.Unmarshal(parts[0], &name) != nil || json.Unmarshal(parts[3], &value) != nil {
			continue
		}
		switch strings.ToLower(name) {
		case "kind":
			c.Kind = strings.ToLower(value)
		case "fn":
			c.FN = strings.TrimSpace(value)
		case "org":
			c.Org = strings.TrimSpace(value)
		}
	}
	return c
}

// Bootstrap is a parsed RFC 9224 registry: entries (TLDs, prefixes or AS
// ranges) mapped to service base URLs.
type Bootstrap struct {
	Services []BootstrapService
}

// BootstrapService is one services[] member.
type BootstrapService struct {
	Entries []string
	URLs    []string
}

// ParseBootstrap decodes a bootstrap file ({"services": [[[entries],
// [urls]], ...]}).
func ParseBootstrap(b []byte) (Bootstrap, error) {
	var raw struct {
		Services [][][]string `json:"services"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return Bootstrap{}, fmt.Errorf("rdap bootstrap: %w", err)
	}
	var bs Bootstrap
	for _, s := range raw.Services {
		if len(s) < 2 {
			continue
		}
		bs.Services = append(bs.Services, BootstrapService{Entries: s[len(s)-2], URLs: s[len(s)-1]})
	}
	if len(bs.Services) == 0 {
		return Bootstrap{}, errors.New("rdap bootstrap: no services")
	}
	return bs, nil
}

// baseURL picks the https URL of a service (else the first), with a
// trailing slash.
func (s BootstrapService) baseURL() string {
	u := ""
	for _, x := range s.URLs {
		if strings.HasPrefix(x, "https://") {
			u = x
			break
		}
	}
	if u == "" && len(s.URLs) > 0 {
		u = s.URLs[0]
	}
	if u != "" && !strings.HasSuffix(u, "/") {
		u += "/"
	}
	return u
}

// ForDomain returns the base URL for the longest matching domain entry.
func (b Bootstrap) ForDomain(name string) string {
	best, bestLen := "", -1
	for _, s := range b.Services {
		for _, e := range s.Entries {
			e = strings.ToLower(strings.Trim(e, "."))
			if (name == e || strings.HasSuffix(name, "."+e)) && len(e) > bestLen {
				best, bestLen = s.baseURL(), len(e)
			}
		}
	}
	return best
}

// ForIP returns the base URL for the most specific prefix containing a.
func (b Bootstrap) ForIP(a netip.Addr) string {
	best, bestBits := "", -1
	for _, s := range b.Services {
		for _, e := range s.Entries {
			p, err := netip.ParsePrefix(e)
			if err != nil {
				continue
			}
			if p.Contains(a.Unmap()) && p.Bits() > bestBits {
				best, bestBits = s.baseURL(), p.Bits()
			}
		}
	}
	return best
}

// ForASN returns the base URL whose range contains n.
func (b Bootstrap) ForASN(n int64) string {
	for _, s := range b.Services {
		for _, e := range s.Entries {
			lo, hi, ok := strings.Cut(e, "-")
			if !ok {
				hi = lo
			}
			l, err1 := strconv.ParseInt(lo, 10, 64)
			h, err2 := strconv.ParseInt(hi, 10, 64)
			if err1 == nil && err2 == nil && n >= l && n <= h {
				return s.baseURL()
			}
		}
	}
	return ""
}

// rdapClient fetches bootstrap files (cached) and objects through an
// intelligence-source client.
type rdapClient struct {
	bootstrapURL string
	mu           sync.Mutex
	cache        map[string]cachedBootstrap
}

type cachedBootstrap struct {
	b  Bootstrap
	at time.Time
}

// DefaultBootstrapURL is the IANA RDAP bootstrap location.
const DefaultBootstrapURL = "https://data.iana.org/rdap/"

const bootstrapTTL = 24 * time.Hour

var errRDAPNotFound = errors.New("rdap: object not found")

func (c *rdapClient) get(ctx context.Context, hc *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/rdap+json, application/json")
	req.Header.Set("User-Agent", "flowforge-rdap/1.0")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, Errorf("unavailable", true, "rdap request failed: "+err.Error())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxRDAPBody+1))
	if err != nil {
		return nil, Errorf("unavailable", true, "rdap response: "+err.Error())
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errRDAPNotFound
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, Errorf("unavailable", true, "rdap server answered "+resp.Status)
	case resp.StatusCode != http.StatusOK:
		return nil, Errorf("invalid_response", false, "rdap server answered "+resp.Status)
	case len(body) > MaxRDAPBody:
		return nil, Errorf("invalid_response", false, "rdap response too large")
	}
	return body, nil
}

func (c *rdapClient) bootstrap(ctx context.Context, hc *http.Client, kind string, now time.Time) (Bootstrap, error) {
	c.mu.Lock()
	if e, ok := c.cache[kind]; ok && now.Sub(e.at) < bootstrapTTL {
		c.mu.Unlock()
		return e.b, nil
	}
	c.mu.Unlock()
	base := c.bootstrapURL
	if base == "" {
		base = DefaultBootstrapURL
	}
	body, err := c.get(ctx, hc, strings.TrimSuffix(base, "/")+"/"+kind+".json")
	if err != nil {
		if errors.Is(err, errRDAPNotFound) {
			return Bootstrap{}, Errorf("unavailable", true, "rdap bootstrap "+kind+" not found")
		}
		return Bootstrap{}, err
	}
	b, err := ParseBootstrap(body)
	if err != nil {
		return Bootstrap{}, Errorf("invalid_response", false, err.Error())
	}
	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[string]cachedBootstrap{}
	}
	c.cache[kind] = cachedBootstrap{b, now}
	c.mu.Unlock()
	return b, nil
}

// object fetches <base><path> and parses it. A missing object or registry
// returns errRDAPNotFound.
func (c *rdapClient) object(ctx context.Context, hc *http.Client, base, path string) (RDAPObject, error) {
	if base == "" {
		return RDAPObject{}, errRDAPNotFound
	}
	body, err := c.get(ctx, hc, base+path)
	if err != nil {
		return RDAPObject{}, err
	}
	o, err := ParseRDAPObject(body)
	if err != nil {
		return RDAPObject{}, Errorf("invalid_response", false, err.Error())
	}
	return o, nil
}

// rdapDate normalizes an RDAP date to RFC 3339 UTC with milliseconds (kept
// as given when it does not parse).
func rdapDate(s string) string {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return timeText(t)
	}
	return s
}

func (o RDAPObject) event(action string) string {
	for _, e := range o.Events {
		if strings.EqualFold(e.Action, action) {
			return rdapDate(e.Date)
		}
	}
	return ""
}

// rdapOrg is an Organization found among an object's entities.
type rdapOrg struct {
	org  oam.Organization
	role string // "registrant" or "administrative"
}

// rdapOrgs returns the organizations in roles registrant and
// administrative (top level and nested, depth 3), deduplicated by (unique
// id, role) in document order. Individuals without an organization name
// and redacted entries are skipped, so no personal data is stored.
func rdapOrgs(es []RDAPEntity) []rdapOrg {
	var out []rdapOrg
	seen := map[string]bool{}
	var walk func([]RDAPEntity, int)
	walk = func(es []RDAPEntity, depth int) {
		for _, e := range es {
			c := ParseVCard(e.VCard)
			name := c.Org
			if name == "" && c.Kind != "individual" {
				name = c.FN
			}
			lname := strings.ToLower(name)
			usable := name != "" && !strings.Contains(lname, "redacted") && !strings.Contains(lname, "privacy") && !strings.Contains(lname, "withheld")
			for _, role := range e.Roles {
				role = strings.ToLower(role)
				if (role != "registrant" && role != "administrative") || !usable {
					continue
				}
				id := "rdap-name:" + lname
				if h := strings.TrimSpace(e.Handle); h != "" && !strings.Contains(strings.ToLower(h), "redacted") {
					id = "rdap:" + strings.ToUpper(h)
				}
				if k := id + "|" + role; !seen[k] {
					seen[k] = true
					out = append(out, rdapOrg{org: oam.Organization{UniqueID: id, Name: name}, role: role})
				}
			}
			if depth < 3 {
				walk(e.Entities, depth+1)
			}
		}
	}
	walk(es, 1)
	return out
}

// rangeCIDRs returns the prefixes exactly covering [start, end] (at most
// 16).
func rangeCIDRs(start, end netip.Addr) []netip.Prefix {
	var out []netip.Prefix
	if !start.IsValid() || !end.IsValid() || start.Is4() != end.Is4() || end.Less(start) {
		return nil
	}
	cur := start
	for len(out) < 16 {
		bits := cur.BitLen()
		for bits > 0 {
			p, _ := cur.Prefix(bits - 1)
			last := lastAddr(p)
			if p.Addr() != cur || end.Less(last) {
				break
			}
			bits--
		}
		p := netip.PrefixFrom(cur, bits)
		out = append(out, p)
		last := lastAddr(p)
		if last == end {
			return out
		}
		cur = last.Next()
		if !cur.IsValid() {
			return out
		}
	}
	return out
}

func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 1 << (7 - i%8)
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

// networkPrefixes returns the CIDRs of an ip network object (cidr0, else
// derived from the address range).
func (o RDAPObject) networkPrefixes() []netip.Prefix {
	var out []netip.Prefix
	for _, c := range o.CIDRs {
		pfx := c.V4Prefix
		if pfx == "" {
			pfx = c.V6Prefix
		}
		if p, err := netip.ParsePrefix(pfx + "/" + strconv.Itoa(c.Length)); err == nil {
			out = append(out, p.Masked())
		}
	}
	if len(out) == 0 {
		s, err1 := netip.ParseAddr(o.StartAddress)
		e, err2 := netip.ParseAddr(o.EndAddress)
		if err1 == nil && err2 == nil {
			out = rangeCIDRs(s, e)
		}
	}
	return out
}

func ipVersion(a netip.Addr) string {
	if a.Unmap().Is4() {
		return "IPv4"
	}
	return "IPv6"
}

// ipNetRecord maps an ip network object.
func (o RDAPObject) ipNetRecord() oam.IPNetRecord {
	r := oam.IPNetRecord{Handle: o.Handle, StartAddress: o.StartAddress, EndAddress: o.EndAddress, NetType: o.Type, Name: o.Name,
		Country: o.Country, ParentHandle: o.ParentHandle, WhoisServer: o.Port43, CreatedDate: o.event("registration"),
		UpdatedDate: o.event("last changed"), Status: o.Status}
	if ps := o.networkPrefixes(); len(ps) > 0 {
		r.CIDR = ps[0].String()
	}
	return r
}

// addOrgs adds the organizations of es with edges from ref.
func addOrgs(b *obsBuilder, ref string, es []RDAPEntity, orgs map[string]oam.Organization) {
	for _, o := range rdapOrgs(es) {
		oref := b.entity("org:"+o.org.UniqueID, o.org)
		b.edge(ref, oref, oam.SimpleRelation{Label: o.role})
		orgs[o.org.UniqueID] = o.org
	}
}

// addNetwork adds an ip network object: IPNetRecord, its Netblocks with
// registration edges, contains edges to addr (when valid) and the
// organizations. It returns the most specific netblock containing addr.
func addNetwork(b *obsBuilder, o RDAPObject, addr netip.Addr, orgs map[string]oam.Organization) (netip.Prefix, bool) {
	rec := o.ipNetRecord()
	if rec.Handle == "" && rec.CIDR == "" {
		return netip.Prefix{}, false
	}
	rref := b.entity("ipnet:"+strings.ToUpper(rec.Handle)+"|"+rec.CIDR, rec)
	var best netip.Prefix
	found := false
	for _, p := range o.networkPrefixes() {
		nref := b.entity("net:"+p.String(), oam.Netblock{CIDR: p.String(), IPVersion: ipVersion(p.Addr())})
		b.edge(nref, rref, oam.SimpleRelation{Label: oam.LabelRegistration})
		if addr.IsValid() && p.Contains(addr) {
			b.edge(nref, b.entity("ip:"+addr.String(), oam.IPAddress{Address: addr.String(), IPVersion: ipVersion(addr)}),
				oam.SimpleRelation{Label: oam.LabelContains})
			if !found || p.Bits() > best.Bits() {
				best, found = p, true
			}
		}
	}
	addOrgs(b, rref, o.Entities, orgs)
	return best, found
}

// autnumNumber returns the AS number of an autnum object.
func (o RDAPObject) autnumNumber() int64 {
	if o.StartAutnum != nil {
		return *o.StartAutnum
	}
	n, _ := strconv.ParseInt(strings.TrimPrefix(strings.ToUpper(o.Handle), "AS"), 10, 64)
	return n
}

// addAutnum adds an autnum object: AutonomousSystem, AutnumRecord with a
// registration edge and the organizations.
func addAutnum(b *obsBuilder, o RDAPObject, orgs map[string]oam.Organization) bool {
	n := o.autnumNumber()
	if n < 1 || n > 1<<32-1 {
		return false
	}
	aref := b.entity("asn:"+strconv.FormatInt(n, 10), oam.AutonomousSystem{Number: n})
	rref := b.entity("autnum:"+strconv.FormatInt(n, 10), oam.AutnumRecord{Number: n, Handle: o.Handle, Name: o.Name, WhoisServer: o.Port43,
		CreatedDate: o.event("registration"), UpdatedDate: o.event("last changed"), Status: o.Status})
	b.edge(aref, rref, oam.SimpleRelation{Label: oam.LabelRegistration})
	addOrgs(b, rref, o.Entities, orgs)
	return true
}

// RDAP is the passive rdap tool (spec 6.4 "WHOIS and RDAP", V2_DESIGN §8):
// it finds the registry through the IANA bootstrap files and fetches the
// domain (for the input's registrable domain) or ip network object,
// following redirects (rdap.org and registry referrals) through the egress
// proxy, which admits only the intelligence-source allowlist. It emits
// DomainRecord or IPNetRecord (+ Netblock) and the registrant and
// administrative Organizations.
type RDAP struct {
	// BootstrapURL defaults to DefaultBootstrapURL.
	BootstrapURL string
	once         sync.Once
	c            *rdapClient
}

func (r *RDAP) client() *rdapClient {
	r.once.Do(func() { r.c = &rdapClient{bootstrapURL: r.BootstrapURL} })
	return r.c
}

// Spec implements Adapter.
func (*RDAP) Spec() Spec {
	return Spec{Name: "rdap", Version: "1.0.0", Description: "RDAP registration data (bootstrap, domain and ip network objects)",
		InputType: "Host", OutputType: "List<Organization>",
		Observes: []string{oam.TypeDomainRecord, oam.TypeIPNetRecord, oam.TypeOrganization, oam.TypeNetblock, oam.TypeFQDN, oam.TypeIPAddress},
		Class:    core.ClassPassive, RateRPS: 2, RateBurst: 4, Concurrency: 2, Timeout: 2 * time.Minute, Idempotent: true}
}

// Run implements Adapter.
func (r *RDAP) Run(ctx context.Context, input json.RawMessage, tc *ToolCtx, out Emitter) (json.RawMessage, error) {
	host, err := decodeHost(input)
	if err != nil {
		return nil, err
	}
	t, err := scope.Normalize(host, scope.TargetHost)
	if err != nil {
		return nil, Errorf("invalid_input", false, "input is not a host")
	}
	kind := scope.TargetHost
	if !t.IsName() {
		kind = scope.TargetIP
	}
	nw := tc.Network()
	if nw == nil {
		return nil, ErrNoEgress
	}
	d, ok, err := tc.Allowed(ctx, out, t.String(), kind, core.ClassPassive)
	if !ok {
		if err != nil {
			return nil, err
		}
		return json.RawMessage("[]"), nil
	}
	hc := nw.IntelClient(tc, d)
	c := r.client()
	b := &obsBuilder{refs: map[string]string{}, obs: oam.Observation{ObservationID: tc.ObservationID("rdap", t.String()), ObservedAt: tc.Now(), Confidence: 0.9}}
	orgs := map[string]oam.Organization{}
	if t.IsName() {
		domain := netnorm.RegistrableDomain(t.Name())
		if domain == "" {
			domain = t.Name()
		}
		bs, err := c.bootstrap(ctx, hc, "dns", tc.Now())
		if err != nil {
			return nil, err
		}
		o, err := c.object(ctx, hc, bs.ForDomain(domain), "domain/"+domain)
		if errors.Is(err, errRDAPNotFound) {
			_ = out.Progress(1, 1, "no registration data")
			return json.RawMessage("[]"), nil
		}
		if err != nil {
			return nil, err
		}
		if o.ObjectClassName != "domain" {
			return nil, Errorf("invalid_response", false, "expected a domain object")
		}
		fref, _ := b.fqdn(domain)
		rec := oam.DomainRecord{ID: o.Handle, Domain: domain, WhoisServer: o.Port43, CreatedDate: o.event("registration"),
			UpdatedDate: o.event("last changed"), ExpirationDate: o.event("expiration"), Status: o.Status}
		if name, ext, ok := strings.Cut(domain, "."); ok {
			rec.Name, rec.Extension = name, ext
		}
		if strings.Contains(domain, "xn--") {
			rec.Punycode = domain
		}
		if o.SecureDNS != nil && o.SecureDNS.DelegationSigned != nil {
			v := *o.SecureDNS.DelegationSigned
			rec.DNSSEC = &v
		}
		rref := b.entity("domrec:"+domain, rec)
		b.edge(fref, rref, oam.SimpleRelation{Label: oam.LabelRegistration})
		addOrgs(b, rref, o.Entities, orgs)
	} else {
		addr := netip.MustParseAddr(t.String())
		kindFile := "ipv4"
		if addr.Is6() {
			kindFile = "ipv6"
		}
		bs, err := c.bootstrap(ctx, hc, kindFile, tc.Now())
		if err != nil {
			return nil, err
		}
		o, err := c.object(ctx, hc, bs.ForIP(addr), "ip/"+addr.String())
		if errors.Is(err, errRDAPNotFound) {
			_ = out.Progress(1, 1, "no registration data")
			return json.RawMessage("[]"), nil
		}
		if err != nil {
			return nil, err
		}
		if o.ObjectClassName != "ip network" {
			return nil, Errorf("invalid_response", false, "expected an ip network object")
		}
		addNetwork(b, o, addr, orgs)
	}
	if err := out.Observation(b.obs); err != nil {
		return nil, err
	}
	return orgsOutput(orgs)
}

// orgsOutput is the prelude List<Organization>, sorted by unique_id.
func orgsOutput(orgs map[string]oam.Organization) (json.RawMessage, error) {
	// PORT: map iteration order is random; ids are sorted so the output is
	// deterministic.
	ids := make([]string, 0, len(orgs))
	for id := range orgs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	list := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		list = append(list, map[string]any{"unique_id": id, "name": orgs[id].Name, "legal_name": nil})
	}
	return json.Marshal(list)
}
