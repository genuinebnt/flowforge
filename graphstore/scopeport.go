package graphstore

import (
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
)

// PolicyScope adapts the section 4 matcher to the graph's scope port
// (spec 5.7): the entity's identity field is checked with Policy.Status in
// passive mode, without resolution. policy returns the program's current
// policy, or nil when none is known (every entity is then unknown).
// Asset types without a network identity (registrations, services,
// certificates, ...) stay unknown: nothing is inferred into scope.
func PolicyScope(policy func(tenantID, programID string) *scope.Policy) oam.ScopeFunc {
	return func(tenantID, programID string, a oam.Asset) oam.ScopeStatus {
		p := policy(tenantID, programID)
		target, kind, ok := ScopeTarget(a)
		if p == nil || !ok {
			return oam.ScopeUnknown
		}
		switch p.Status(target, kind) {
		case scope.StatusInScope:
			return oam.ScopeInScope
		case scope.StatusOutOfScope:
			return oam.ScopeOutOfScope
		}
		return oam.ScopeUnknown
	}
}

// ScopeTarget returns the scope target an asset is classified by: the
// FQDN name, IP address, netblock CIDR or URL.
func ScopeTarget(a oam.Asset) (string, scope.TargetKind, bool) {
	switch v := a.(type) {
	case oam.FQDN:
		return v.Name, scope.TargetHost, v.Name != ""
	case oam.IPAddress:
		return v.Address, scope.TargetIP, v.Address != ""
	case oam.Netblock:
		return v.CIDR, scope.TargetCIDR, v.CIDR != ""
	case oam.URL:
		return v.URL, scope.TargetURL, v.URL != ""
	}
	return "", scope.TargetUnspecified, false
}
