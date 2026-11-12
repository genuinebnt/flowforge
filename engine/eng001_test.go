package engine_test

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/engine/enginetest"
	"github.com/genuinebnt/flowforge/testkit"
)

var update = flag.Bool("update", false, "rewrite the golden files in conformance/engine")

// reconHandlers answers the recon tools: subfinder finds three names (one
// out of scope), httpx returns a service per host plus an observation.
func reconHandlers(h *enginetest.Harness) {
	h.Exec.Handle("subfinder", enginetest.Output([]string{"api.acme.com", "evil.org", "www.acme.com"}))
	h.Exec.Handle("httpx", func(ctx contextT, call engine.ToolCall) (engine.ToolResult, error) {
		var host string
		_ = jsonUnmarshal(call.Input, &host)
		out := map[string]any{"url": "https://" + host + "/", "status": 200}
		obs := map[string]any{"entities": []any{map[string]any{"ref": "u1", "asset": map[string]any{"type": "URL", "url": "https://" + host + "/"}}}, "edges": []any{}}
		return engine.ToolResult{Output: mustJSON(out), Observations: []jsonRaw{mustJSON(obs)}}, nil
	})
}

func TestEng001_HappyPath(t *testing.T) {
	h := enginetest.New(t)
	reconHandlers(h)
	c := enginetest.Analyst(enginetest.TenantA)
	v := h.Register(c, "recon", enginetest.Recon, enginetest.ProgramA)
	if v.Version != 1 || v.InferredClass != core.ClassActive {
		t.Fatalf("version %+v", v)
	}
	run := h.Submit(c, "recon", enginetest.ProgramA, map[string]any{"roots": []string{"acme.com"}}, "key-1")
	w := h.Engine.NewWorker("w1")
	if st := h.RunToEnd(w, run.ID); st != engine.StatusSucceeded {
		t.Fatalf("status %s\n%s", st, enginetest.NormalJSON(enginetest.Normalize(h.Events(run.ID))))
	}
	got, err := h.Engine.GetRun(h.Ctx(), c, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Output) != `{"alerts":1,"new_assets":2}` || got.CompensationOutcome != engine.CompensationNone {
		t.Fatalf("run %+v output %s", got.Status, got.Output)
	}
	if n := len(h.Exec.CallsTo("httpx")); n != 2 {
		t.Fatalf("httpx calls %d, want 2 (evil.org is out of scope)", n)
	}
	if h.Ingester.Unique() != 2 {
		t.Fatalf("observations %d", h.Ingester.Unique())
	}
	if n := len(h.Pub.Envelopes(engine.TopicRunEvents)); uint64(n) != got.LastSeq {
		t.Fatalf("published %d run events, log has %d", n, got.LastSeq)
	}
	checkGolden(t, "eng_001_recon.golden.json", enginetest.NormalJSON(enginetest.Normalize(h.Events(run.ID))))
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(testkit.ConformanceDir(t, "engine"), name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(want) != got {
		t.Fatalf("event log differs from %s (run with -update after checking):\n%s", name, got)
	}
}
