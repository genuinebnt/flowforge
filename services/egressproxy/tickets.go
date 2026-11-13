package egressproxy

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/services/internal/platform"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
)

// session is what a ticket stands for.
type session struct {
	id       platform.Identity
	decision string
	runID    string
	path     string
	ports    []int
	rec      scopesvc.DecisionRecord
	expires  time.Time
}

// tickets is the in-memory ticket store. Tickets live for at most
// TicketTTL; a proxy replica only knows its own tickets (clients send the
// SOCKS and DNS traffic to the replica that issued them).
type tickets struct {
	mu sync.Mutex
	m  map[string]*session
}

// maxTickets bounds the store; issuing fails closed beyond it.
const maxTickets = 1 << 16

func (t *tickets) put(s *session, now time.Time) (string, bool) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", false
	}
	tk := "tk_" + base64.RawURLEncoding.EncodeToString(b[:])
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]*session{}
	}
	if len(t.m) >= maxTickets {
		// PORT: map iteration order does not matter: every expired entry goes.
		for k, v := range t.m {
			if !now.Before(v.expires) {
				delete(t.m, k)
			}
		}
		if len(t.m) >= maxTickets {
			return "", false
		}
	}
	t.m[tk] = s
	return tk, true
}

func (t *tickets) get(tk string, now time.Time) *session {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.m[tk]
	if s == nil {
		return nil
	}
	if !now.Before(s.expires) {
		delete(t.m, tk)
		return nil
	}
	return s
}

// serveTicket issues a ticket after the same validation as a proxied
// request, minus the target: token, decision (tenant, allow, age, run),
// declared ports and kill switch.
func (p *Proxy) serveTicket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodPost || r.URL.Path != TicketPath {
		p.refused(ctx, w, nil, r.URL.String(), &refusal{reason: "invalid_target", detail: "unknown control endpoint", status: http.StatusNotFound})
		return
	}
	rq, rf := p.identify(ctx, r.Header)
	if rf == nil {
		rf = p.loadDecision(ctx, rq)
	}
	if rf == nil {
		rf = p.killSwitch(ctx, rq)
	}
	if rf != nil {
		p.refused(ctx, w, rq, "ticket", rf)
		return
	}
	now := p.Now()
	ttl := min(p.TicketTTL, p.MaxDecisionAge-now.Sub(rq.rec.Decision.DecidedAt))
	if ttl <= 0 {
		p.refused(ctx, w, rq, "ticket", refuse("decision_expired", "decision is too old"))
		return
	}
	s := &session{id: rq.id, decision: rq.decision, runID: rq.runID, path: rq.path, ports: rq.ports, rec: rq.rec, expires: now.Add(ttl)}
	tk, ok := p.tickets.put(s, now)
	if !ok {
		p.refused(ctx, w, rq, "ticket", &refusal{reason: "tickets_exhausted", detail: "ticket store full", status: http.StatusServiceUnavailable})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(TicketResponse{Ticket: tk, DecisionID: rq.decision, ExpiresInMs: ttl.Milliseconds(),
		SOCKS5: p.SOCKSAddr, DNS: p.DNSAddr})
}

// request rebuilds the caller identity a ticket was issued to; the decision
// itself is reloaded and re-checked on every use.
func (s *session) request() *request {
	return &request{id: s.id, decision: s.decision, runID: s.runID, path: s.path, ports: s.ports}
}
