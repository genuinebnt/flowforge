package egressproxy

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/scope"
)

// The wire contract between sandboxed tools (the tool runner) and the
// proxy, beyond the headers in services/internal/platform.
const (
	// ControlHost is the reserved host of the proxy's control endpoints,
	// reached as absolute-URI requests through the HTTP proxy port (.invalid
	// never resolves, RFC 6761, so it can never be a real target).
	ControlHost = "egress.flowforge.invalid"
	// TicketPath issues a ticket: POST http://ControlHost/v1/tickets with
	// the same headers as a proxied request (token, decision, run, effect
	// path, optional ports).
	TicketPath = "/v1/tickets"
	// HeaderPorts declares the TCP ports a tool call may use under a
	// host-level decision ("22,80,443"). It can only narrow the program's
	// port policy, never widen it.
	HeaderPorts = "X-Flowforge-Ports"
	// HeaderRetryAfterMs is set on rate-limit refusals (429) next to
	// Retry-After, in milliseconds.
	HeaderRetryAfterMs = "X-Flowforge-Retry-After-Ms"
	// EDNSTicketOption is the EDNS(0) option code (local/experimental range,
	// RFC 6891 §9) carrying the ticket in DNS queries to the forwarder.
	EDNSTicketOption = 65280
	// ReasonIntelSource is the decision reason recorded when a passive
	// decision reaches an allowlisted intelligence source.
	ReasonIntelSource = "intel_source"
)

// TicketResponse is the JSON body of a ticket. A ticket authenticates SOCKS5
// (username = decision id, password = ticket; RFC 1929 limits both to 255
// octets, too short for an internal token) and DNS queries (EDNS option
// EDNSTicketOption). It is bound to the caller's identity, the decision,
// the run, the effect path and the declared ports, and expires after the
// proxy's TicketTTL (default 60 s) or when the decision reaches
// MaxDecisionAge, whichever comes first.
type TicketResponse struct {
	Ticket      string `json:"ticket"`
	DecisionID  string `json:"decision_id"`
	ExpiresInMs int64  `json:"expires_in_ms"`
	SOCKS5      string `json:"socks5,omitempty"`
	DNS         string `json:"dns,omitempty"`
}

// FormatPorts renders ports for HeaderPorts.
func FormatPorts(ports []int) string {
	s := make([]string, len(ports))
	for i, p := range ports {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ",")
}

// ParsePorts parses HeaderPorts: comma-separated ports 1..65535, at most
// 1000, returned sorted and deduplicated. An empty header declares nothing
// (nil, true); a malformed one reports false, which the proxy refuses.
func ParsePorts(h string) ([]int, bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return nil, true
	}
	parts := strings.Split(h, ",")
	if len(parts) > 1000 {
		return nil, false
	}
	out := make([]int, 0, len(parts))
	for _, s := range parts {
		p, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || p < 1 || p > 65535 {
			return nil, false
		}
		out = append(out, p)
	}
	slices.Sort(out)
	return slices.Compact(out), true
}

// PortRange is an inclusive range of TCP ports.
type PortRange struct{ From, To int }

// Ports returns the single-port ranges of ps.
func Ports(ps ...int) []PortRange {
	out := make([]PortRange, len(ps))
	for i, p := range ps {
		out[i] = PortRange{p, p}
	}
	return out
}

// DefaultPorts is the port policy when none is configured: the web ports
// only (fail closed for everything else).
var DefaultPorts = Ports(80, 443)

// CommonPorts is a wider policy for development stacks: common service
// ports a port scan or certificate grab may try.
var CommonPorts = Ports(21, 22, 23, 25, 53, 80, 110, 143, 443, 445, 465, 587, 993, 995, 1433, 1521, 2375, 3000, 3306,
	3389, 5000, 5432, 5672, 5900, 6379, 6443, 8000, 8008, 8080, 8081, 8443, 8888, 9000, 9090, 9200, 9443, 11211, 27017)

// PortPolicy says which TCP ports a host-level (Host or IP) decision
// authorizes. A decision is host-level in the scope service; the proxy
// makes it cover (host, port) only for ports in the program's policy and,
// when the tool call declared ports (HeaderPorts or the ticket), in that
// declaration too. URL decisions cover exactly their URL's port.
type PortPolicy struct {
	// Default applies to programs without an entry (nil: DefaultPorts).
	Default []PortRange
	// Programs overrides the policy per program id (core.UUID string).
	Programs map[string][]PortRange
}

// Allowed reports whether port is in program's policy.
func (pp *PortPolicy) Allowed(program string, port int) bool {
	rs := DefaultPorts
	if pp != nil {
		if r, ok := pp.Programs[program]; ok {
			rs = r
		} else if pp.Default != nil {
			rs = pp.Default
		}
	}
	for _, r := range rs {
		if port >= r.From && port <= r.To {
			return true
		}
	}
	return false
}

// IntelSource is one allowlisted intelligence source: a public service that
// passive tools query about program assets (crt.sh, RDAP, Team Cymru). The
// target of such a request is the source, not a program asset, so it is
// checked against this list rather than the program scope, and only for
// decisions of class passive.
type IntelSource struct {
	// Host is the source's name (normalized as a scope host).
	Host string
	// Subdomains also admits every name below Host (DNS zones such as
	// origin.asn.cymru.com).
	Subdomains bool
	// Ports are the TCP ports (default 443 and 80).
	Ports []int
	// Addrs, when set, pins the addresses to connect to (operator
	// configuration, trusted as given; tests use loopback). Otherwise the
	// proxy resolves Host with IntelResolver and refuses internal addresses.
	Addrs []netip.Addr
	// Rate optionally replaces the program rate for this source (rl:h key of
	// the source host).
	Rate scope.Rate
}

func (s IntelSource) matches(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(s.Host), ".")
	return host == h || (s.Subdomains && strings.HasSuffix(host, "."+h))
}

func (s IntelSource) port(p int) bool {
	if len(s.Ports) == 0 {
		return p == 443 || p == 80
	}
	return slices.Contains(s.Ports, p)
}

// DefaultIntelSources are the public sources the passive tools use: crt.sh,
// the IANA RDAP bootstrap registry, rdap.org, the five RIR RDAP services,
// the largest gTLD registries' RDAP services and Team Cymru (whois and its
// DNS zones). Registries missing here are refused until added.
func DefaultIntelSources() []IntelSource {
	hosts := []string{"crt.sh", "data.iana.org", "rdap.iana.org", "rdap.org", "rdap.arin.net", "rdap.db.ripe.net",
		"rdap.apnic.net", "rdap.lacnic.net", "rdap.afrinic.net", "rdap.verisign.com", "rdap.publicinterestregistry.org",
		"rdap.identitydigital.services", "pubapi.registry.google", "rdap.centralnic.com"}
	out := make([]IntelSource, 0, len(hosts)+2)
	for _, h := range hosts {
		out = append(out, IntelSource{Host: h, Ports: []int{443}})
	}
	return append(out,
		IntelSource{Host: "whois.cymru.com", Ports: []int{43}},
		IntelSource{Host: "asn.cymru.com", Subdomains: true, Ports: []int{43}})
}
