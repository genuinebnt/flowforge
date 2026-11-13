package alerts

import (
	"sort"
	"time"
)

// Durable throttle and digest state (spec 15.3; G-LIM-06). A Processor
// keeps its windows in memory; Outcome.State lists every window an event
// changed, so the caller can persist it in the same transaction as the
// alert events (the notifier does), and Restore rebuilds a processor from
// what was persisted. Without this a restart re-fires a throttled key
// (inv_08 in production) and drops open digests.

// ThrottleWindow is one dedupe key's open throttle window.
type ThrottleWindow struct {
	Key        string
	Until      time.Time
	Suppressed int // matches suppressed in this window so far
}

// StateChange is what one Process call changed: throttle windows to
// upsert and open digests to upsert (their full current contents). A
// digest Flush returns is closed: its persisted copy is to be deleted
// (by Notification.Digest.RuleID).
type StateChange struct {
	Throttle []ThrottleWindow
	Digests  []Digest
}

// Empty reports whether c changes nothing.
func (c StateChange) Empty() bool { return len(c.Throttle) == 0 && len(c.Digests) == 0 }

// ProcessorState is everything Restore needs.
type ProcessorState struct {
	Throttle []ThrottleWindow
	Digests  []Digest
}

// window returns key's window.
func (t *Throttle) window(key string) (ThrottleWindow, bool) {
	st, ok := t.keys[key]
	if !ok {
		return ThrottleWindow{}, false
	}
	return ThrottleWindow{Key: key, Until: st.until, Suppressed: st.suppressed}, true
}

// restore installs w, keeping the later window if key already has one.
func (t *Throttle) restore(w ThrottleWindow) {
	if cur, ok := t.keys[w.Key]; ok && !w.Until.After(cur.until) {
		return
	}
	t.keys[w.Key] = &throttleState{until: w.Until, suppressed: w.Suppressed}
}

// Restore loads persisted state into a processor built for the same rule
// set. Windows that already ended are skipped (their suppressed count only
// fed SuppressedBefore of the next alert, which is best effort after a
// restart). A digest whose rule is no longer in the set is dropped, and a
// kept digest takes its actions and quiet hours from the current rule
// (they are not persisted: rule changes apply to open digests).
func (p *Processor) Restore(s ProcessorState) {
	now := p.clock.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, w := range s.Throttle {
		if now.Before(w.Until) {
			p.throttle.restore(w)
		}
	}
	for _, d := range s.Digests {
		r, ok := p.rules.Rule(d.RuleID)
		if !ok || r.Digest <= 0 {
			continue
		}
		d.Actions = r.Actions
		dg := d
		dg.Items = append([]Payload(nil), d.Items...)
		p.digests.open[d.RuleID] = &dg
		p.quiet[d.RuleID] = r.QuietHours
	}
}

// State returns a copy of the processor's open windows and digests,
// sorted by key and rule id.
func (p *Processor) State() ProcessorState {
	p.mu.Lock()
	defer p.mu.Unlock()
	var s ProcessorState
	for k := range p.throttle.keys {
		w, _ := p.throttle.window(k)
		s.Throttle = append(s.Throttle, w)
	}
	for _, d := range p.digests.open {
		c := *d
		c.Items = append([]Payload(nil), d.Items...)
		s.Digests = append(s.Digests, c)
	}
	sort.Slice(s.Throttle, func(i, j int) bool { return s.Throttle[i].Key < s.Throttle[j].Key })
	sort.Slice(s.Digests, func(i, j int) bool { return s.Digests[i].RuleID < s.Digests[j].RuleID })
	return s
}
