package engine

import (
	"fmt"

	"github.com/genuinebnt/flowforge/eventstore"
)

// maybeSnapshot takes a VM snapshot of the workflow body when the root VM
// is suspended at the live frontier and SnapshotEveryEffects effects or
// SnapshotInterval have passed since the last one (spec 8.7). A snapshot
// is a cache of replay: restoring it yields the VM state replay would
// reach, so dropping snapshots never changes behaviour.
func (r *runner) maybeSnapshot() error {
	l := r.log
	since := l.lastSnapshotAt
	if since.IsZero() {
		since = l.startedAt
	}
	due := l.effectsSinceSnapshot >= r.e.cfg.SnapshotEveryEffects ||
		(!since.IsZero() && r.now().Sub(since) >= r.e.cfg.SnapshotInterval && l.effectsSinceSnapshot > 0)
	if !due {
		return nil
	}
	state, err := r.root.vm.Snapshot()
	if err != nil {
		return nil // not snapshottable here (never fatal: snapshots are a cache)
	}
	h := r.lm.prog.Hash()
	at := l.head
	if err := r.e.d.Store.SaveSnapshot(r.ctx, r.run, eventstore.Snapshot{AtSeq: at, ModuleSHA256: h[:], State: state}); err != nil {
		return nil
	}
	id := derivedID("snapshot", r.run.String(), fmt.Sprint(at))
	return r.append(ev(EvSnapshotTaken, ActorEngine, SnapshotTakenPayload{AtSeq: at, SnapshotID: id.String()}))
}
