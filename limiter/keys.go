package limiter

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Key builders for the hierarchy of spec 14.3. Every component is escaped
// ('%' as "%25", ':' as "%3A") so that distinct component tuples always
// yield distinct keys, even for IPv6 addresses or identifiers that
// contain colons. Components are otherwise used as given: callers pass
// normalized values (for example netnorm.RegistrableDomain of a host).

// GlobalKey returns rl:g:<class>, the platform-wide limit of a class.
func GlobalKey(class string) string { return join("rl:g", class) }

// TenantKey returns rl:t:<tenant_id>:<class>, a tenant's plan limit.
func TenantKey(tenantID, class string) string { return join("rl:t", tenantID, class) }

// ProgramKey returns rl:p:<program_id>, the program's activity.rate.
func ProgramKey(programID string) string { return join("rl:p", programID) }

// HostKey returns rl:h:<program_id>:<target>, where target is the
// registrable domain or IP address being contacted. target is lowercased.
func HostKey(programID, target string) string {
	return join("rl:h", programID, strings.ToLower(target))
}

// ToolKey returns rl:tool:<tenant_id>:<tool>, the ToolSpec rate.
func ToolKey(tenantID, tool string) string { return join("rl:tool", tenantID, tool) }

// AuthIPKey returns rl:auth:ip:<ip>:<endpoint> for auth throttles (13.1).
func AuthIPKey(ip, endpoint string) string { return join("rl:auth:ip", ip, endpoint) }

// AuthAccountKey returns rl:auth:acct:<sha256 email>, where the hash is the
// lowercase hex SHA-256 of the trimmed, lowercased email address. Hashing
// keeps addresses out of the limiter store.
func AuthAccountKey(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return "rl:auth:acct:" + hex.EncodeToString(sum[:])
}

// APIKey returns rl:api:<tenant_id>:<route_class> for REST rate limits.
func APIKey(tenantID, routeClass string) string { return join("rl:api", tenantID, routeClass) }

func join(prefix string, parts ...string) string {
	var b strings.Builder
	b.WriteString(prefix)
	for _, p := range parts {
		b.WriteByte(':')
		for i := 0; i < len(p); i++ {
			switch p[i] {
			case '%':
				b.WriteString("%25")
			case ':':
				b.WriteString("%3A")
			default:
				b.WriteByte(p[i])
			}
		}
	}
	return b.String()
}

// ToolCall names the hierarchy levels a tool call is charged against.
type ToolCall struct {
	Class     string // activity or resource class, for rl:g and rl:t
	TenantID  string
	ProgramID string
	// Target is the registrable domain or IP the tool contacts; empty for
	// tools that contact no target (then no rl:h key is charged).
	Target string
	Tool   string
}

// Requests returns the requests a tool call acquires atomically with
// AcquireAll (spec 14.3): global, tenant, program, target (if any) and
// tool, each with cost. The strictest key decides. A missing required
// component is an error, never a silently skipped level.
func (tc ToolCall) Requests(cost int) ([]Request, error) {
	required := [...]struct{ name, v string }{
		{"class", tc.Class}, {"tenant", tc.TenantID}, {"program", tc.ProgramID}, {"tool", tc.Tool},
	}
	for _, r := range required {
		if r.v == "" {
			return nil, fmt.Errorf("%w: tool call without %s", ErrInvalidKey, r.name)
		}
	}
	reqs := []Request{
		{GlobalKey(tc.Class), cost},
		{TenantKey(tc.TenantID, tc.Class), cost},
		{ProgramKey(tc.ProgramID), cost},
	}
	if tc.Target != "" {
		reqs = append(reqs, Request{HostKey(tc.ProgramID, tc.Target), cost})
	}
	return append(reqs, Request{ToolKey(tc.TenantID, tc.Tool), cost}), nil
}
