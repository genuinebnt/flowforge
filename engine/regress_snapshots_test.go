package engine_test

import (
	"testing"

	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/engine/enginetest"
	"github.com/genuinebnt/flowforge/eventstore"
)

// TestRegress_GENG04_FinishedRunKeepsNoSnapshots: a run that took VM
// snapshots while it ran has none once it finishes (spec 10.7: "snapshots
// keep the last 3 per run and delete when the run finishes").
func TestRegress_GENG04_FinishedRunKeepsNoSnapshots(t *testing.T) {
	h, r := multiHarness(t, func(h *enginetest.Harness) { h.Config.SnapshotEveryEffects = 1 })
	w := h.Engine.NewWorker("w")
	if st := h.RunToEnd(w, r.ID); st != engine.StatusSucceeded {
		t.Fatalf("status %s", st)
	}
	if _, ok, err := h.Store.LoadSnapshot(h.Ctx(), eventstore.RunID(r.ID)); err != nil || ok {
		t.Fatalf("finished run still has a snapshot (ok=%v, err=%v)", ok, err)
	}
}
