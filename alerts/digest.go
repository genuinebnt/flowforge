package alerts

import (
	"sort"
	"time"
)

// DigestMaxItems is how many matches a digest message lists (spec 15.3).
const DigestMaxItems = 20

// Digest is one digest message: every match of a rule collected over the
// rule's digest window.
type Digest struct {
	RuleID      string    `json:"rule_id"`
	Rule        string    `json:"rule"`
	Severity    Severity  `json:"severity"` // highest severity among the items
	Count       int       `json:"count"`
	Items       []Payload `json:"items"` // the first DigestMaxItems matches, in arrival order
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	Actions     []Action  `json:"-"`
}

// Digester collects matches per rule. A rule's window opens with its
// first match and closes d later; Due returns the closed windows. It is
// not safe for concurrent use; Processor serializes access.
type Digester struct {
	open map[string]*Digest
}

// NewDigester returns an empty digester.
func NewDigester() *Digester { return &Digester{open: map[string]*Digest{}} }

// Add records m at now for a rule whose digest window is d.
func (g *Digester) Add(m Match, d time.Duration, now time.Time) {
	dg, ok := g.open[m.RuleID]
	if !ok {
		dg = &Digest{
			RuleID: m.RuleID, Rule: m.RuleName, Severity: m.Severity,
			WindowStart: now, WindowEnd: now.Add(d), Actions: m.Actions,
		}
		g.open[m.RuleID] = dg
	}
	dg.Count++
	if m.Severity > dg.Severity {
		dg.Severity = m.Severity
	}
	if len(dg.Items) < DigestMaxItems {
		dg.Items = append(dg.Items, m.Payload)
	}
}

// Due removes and returns the digests whose window ended at or before
// now, ordered by window end then rule id.
func (g *Digester) Due(now time.Time) []Digest {
	var out []Digest
	for id, dg := range g.open {
		if !now.Before(dg.WindowEnd) {
			out = append(out, *dg)
			delete(g.open, id)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].WindowEnd.Equal(out[j].WindowEnd) {
			return out[i].WindowEnd.Before(out[j].WindowEnd)
		}
		return out[i].RuleID < out[j].RuleID
	})
	return out
}

// Pending returns the number of open digest windows.
func (g *Digester) Pending() int { return len(g.open) }

// NextDue returns the earliest open window end, if any, so a scheduler
// knows when to call Due next.
func (g *Digester) NextDue() (time.Time, bool) {
	var best time.Time
	for _, dg := range g.open {
		if best.IsZero() || dg.WindowEnd.Before(best) {
			best = dg.WindowEnd
		}
	}
	return best, !best.IsZero()
}
