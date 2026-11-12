package scope

import "context"

// Hop is one step of a redirect or CNAME chain: the Location of an HTTP
// redirect (usually TargetURL) or a CNAME target (TargetHost).
type Hop struct {
	Target string     `json:"target"`
	Kind   TargetKind `json:"kind,omitempty"`
}

// ChainResult is the outcome of CheckChain.
type ChainResult struct {
	// Decisions holds the decision for the initial request followed by one
	// per hop checked. Checking stops at the first deny, so a denied chain
	// ends with that deny.
	Decisions []Decision
	// Allowed is true only when the request and every hop were allowed.
	Allowed bool
}

// Final returns the last decision made, which is the deny that stopped
// the chain when Allowed is false.
func (r ChainResult) Final() Decision {
	if len(r.Decisions) == 0 {
		return Decision{}
	}
	return r.Decisions[len(r.Decisions)-1]
}

// CheckChain re-runs Check on every hop of a redirect or CNAME chain
// (spec 4.5), with req's class, time and kill switch, and stops at the
// first deny: a redirect to an out-of-scope host is not followed. Each
// allowed hop carries its own pinned addresses.
func (p *Policy) CheckChain(ctx context.Context, req Request, hops []Hop) ChainResult {
	res := ChainResult{Decisions: make([]Decision, 0, len(hops)+1)}
	d := p.Check(ctx, req)
	res.Decisions = append(res.Decisions, d)
	if !d.Allowed() {
		return res
	}
	for _, h := range hops {
		hr := req
		hr.Target, hr.Kind = h.Target, h.Kind
		d = p.Check(ctx, hr)
		res.Decisions = append(res.Decisions, d)
		if !d.Allowed() {
			return res
		}
	}
	res.Allowed = true
	return res
}
