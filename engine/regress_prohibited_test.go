package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/engine/enginetest"
	"github.com/genuinebnt/flowforge/scope"
)

// G-CORE-09: the scope document's activity.prohibited list (spec 4.1) was
// stored and passed around but enforced nowhere. These tests pin its
// enforcement at registration (compile), submission, scope re-validation
// and before every tool effect (scope OPEN_QUESTIONS 18).

// prohibitingDoc is the harness document (acme.com and example.com up to
// intrusive) with an activity.prohibited list.
func prohibitingDoc(prohibited ...string) scope.Document {
	doc := enginetest.DocAllowing(core.ClassIntrusive, "acme.com", "example.com")
	doc.Activity.Prohibited = prohibited
	return doc
}

func TestRegress_G_CORE_09_CompileRejectsProhibitedTool(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prohibited []string
		wantErr    bool
	}{
		{"tool_name", []string{"dos", "httpx"}, true},
		{"tool_name_case_and_space", []string{" HTTPX "}, true},
		{"effect_class", []string{"active"}, true},
		{"read_effect", []string{"read"}, true}, // echo is a read tool
		{"unrelated", []string{"dos", "social_engineering", "intrusive"}, false},
		{"empty_entry", []string{""}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := enginetest.New(t)
			c := enginetest.Analyst(enginetest.TenantA)
			if _, err := h.Scope.PutScope(enginetest.TenantA, enginetest.ProgramA, prohibitingDoc(tc.prohibited...)); err != nil {
				t.Fatal(err)
			}
			h.Compiler.Add("source:multi", enginetest.Multi)
			_, err := h.Engine.RegisterWorkflow(h.Ctx(), c, engine.RegisterWorkflowRequest{Name: "multi", Source: "source:multi", ProgramID: enginetest.ProgramA})
			reqs := h.Compiler.Requests
			if len(reqs) == 0 || reqs[len(reqs)-1].Policy == nil || strings.Join(reqs[len(reqs)-1].Policy.Prohibited, ",") != strings.Join(tc.prohibited, ",") {
				t.Fatalf("the compile request did not carry activity.prohibited %q", tc.prohibited)
			}
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "E0402") && !hasDiag(err, "E0402") {
					t.Fatalf("register: %v, want E0402", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("register: %v", err)
			}
		})
	}
}

// hasDiag reports whether err carries a diagnostic with the code.
func hasDiag(err error, code string) bool {
	b, _ := json.Marshal(err)
	return strings.Contains(string(b), code)
}

func TestRegress_G_CORE_09_SubmitRejectsProhibitedTool(t *testing.T) {
	h := enginetest.New(t)
	c := enginetest.Analyst(enginetest.TenantA)
	// Registered against a document without a prohibited list.
	h.Register(c, "multi", enginetest.Multi, enginetest.ProgramA)

	// A new scope version forbids the active httpx: submission refuses it.
	if _, err := h.Scope.PutScope(enginetest.TenantA, enginetest.ProgramA, prohibitingDoc("active")); err != nil {
		t.Fatal(err)
	}
	_, err := h.SubmitErr(c, "multi", enginetest.ProgramA, map[string]any{"roots": []string{"api.acme.com"}}, "k1", 0)
	wantReason(t, err, core.KindPermissionDenied, core.ReasonScopeDenied)
	if !strings.Contains(err.Error(), "httpx") {
		t.Fatalf("error %v does not name the tool", err)
	}

	// A later version that prohibits only techniques no tool carries
	// accepts it again.
	if _, err := h.Scope.PutScope(enginetest.TenantA, enginetest.ProgramA, prohibitingDoc("dos", "credential_stuffing")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SubmitErr(c, "multi", enginetest.ProgramA, map[string]any{"roots": []string{"api.acme.com"}}, "k2", 0); err != nil {
		t.Fatalf("submit with unrelated prohibitions: %v", err)
	}
}

// swappedScope serves, once swap is set, a different document for every
// version of the program: a scope service whose answer for the pinned
// version changed after the run was submitted.
type swappedScope struct {
	*enginetest.MemScope
	mu   sync.Mutex
	swap *scope.Document
}

func (s *swappedScope) Policy(ctx context.Context, tenant, program core.UUID, version int32) (*scope.Policy, error) {
	s.mu.Lock()
	doc := s.swap
	s.mu.Unlock()
	if doc == nil {
		return s.MemScope.Policy(ctx, tenant, program, version)
	}
	d := *doc
	d.ProgramID, d.Version = program.String(), int(version)
	return scope.Compile(d)
}

func TestRegress_G_CORE_09_EffectTimeRejectsProhibitedTool(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prohibited []string
		want       engine.RunStatus
	}{
		{"prohibited", []string{"echo"}, engine.StatusFailed},
		{"allowed", []string{"dos", "intrusive"}, engine.StatusSucceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, r := multiHarness(t)
			h.Drain(h.Engine.NewWorker("w")) // httpx and three echo calls done; waits on sleep(1s)
			echoes := len(h.Exec.CallsTo("echo"))

			sc := &swappedScope{MemScope: h.Scope}
			d := h.Deps(h.Store)
			d.Scope = sc
			e, err := engine.New(d, h.Config)
			if err != nil {
				t.Fatal(err)
			}
			doc := prohibitingDoc(tc.prohibited...)
			sc.mu.Lock()
			sc.swap = &doc
			sc.mu.Unlock()

			st := h.RunToEnd(e.NewWorker("w2"), r.ID)
			if st != tc.want {
				t.Fatalf("status %s, want %s; events %v", st, tc.want, enginetest.Types(h.Events(r.ID)))
			}
			if tc.want == engine.StatusSucceeded {
				return
			}
			if n := len(h.Exec.CallsTo("echo")); n != echoes {
				t.Fatalf("the prohibited echo ran (%d calls, %d before)", n, echoes)
			}
			var failed *engine.EffectFailedPayload
			for _, ev := range h.Events(r.ID) {
				if ev.Type != engine.EvEffectFailed {
					continue
				}
				var p engine.EffectFailedPayload
				if err := json.Unmarshal(ev.Payload, &p); err != nil {
					t.Fatal(err)
				}
				failed = &p
			}
			if failed == nil || failed.Error.Code != "scope_denied" || failed.FailCode != "scope_denied" ||
				!strings.Contains(failed.Error.Message, scope.ReasonClassExceedsProgram) || !strings.Contains(failed.Error.Message, "echo") {
				t.Fatalf("effect_failed %+v, want scope_denied naming echo and %s", failed, scope.ReasonClassExceedsProgram)
			}
		})
	}
}

// policyOutageScope fails every Policy call while down is set.
type policyOutageScope struct {
	*enginetest.MemScope
	mu   sync.Mutex
	down bool
}

func (s *policyOutageScope) Policy(ctx context.Context, tenant, program core.UUID, version int32) (*scope.Policy, error) {
	s.mu.Lock()
	down := s.down
	s.mu.Unlock()
	if down {
		return nil, enginetest.ErrScopeUnavailable
	}
	return s.MemScope.Policy(ctx, tenant, program, version)
}

// The effect-time check fails closed: while the policy cannot be loaded
// no tool call starts; the run waits and finishes once it can.
func TestRegress_G_CORE_09_EffectTimeFailsClosed(t *testing.T) {
	h := enginetest.New(t)
	h.Exec.Handle("echo", enginetest.Echo())
	c := enginetest.Analyst(enginetest.TenantA)
	h.Register(c, "chain", enginetest.Chain(1), enginetest.ProgramA)
	r := h.Submit(c, "chain", enginetest.ProgramA, map[string]any{}, "k")

	sc := &policyOutageScope{MemScope: h.Scope, down: true}
	d := h.Deps(h.Store)
	d.Scope = sc
	e, err := engine.New(d, h.Config)
	if err != nil {
		t.Fatal(err)
	}
	w := e.NewWorker("w")
	h.Drain(w)
	if n := len(h.Exec.Calls()); n != 0 {
		t.Fatalf("%d tool calls ran while the scope policy was unavailable", n)
	}
	sc.mu.Lock()
	sc.down = false
	sc.mu.Unlock()
	if st := h.RunToEnd(w, r.ID); st != engine.StatusSucceeded {
		t.Fatalf("status %s, want succeeded; events %v", st, enginetest.Types(h.Events(r.ID)))
	}
}

func TestRegress_G_CORE_09_RevalidationPausesProhibitedTool(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prohibited []string
		want       engine.RunStatus
	}{
		{"prohibited", []string{"ECHO"}, engine.StatusPaused},
		{"allowed", []string{"dos"}, engine.StatusSucceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, r := multiHarness(t)
			w := h.Engine.NewWorker("w")
			h.Drain(w) // waits on sleep(1s)
			echoes := len(h.Exec.CallsTo("echo"))
			v2, err := h.Scope.PutScope(enginetest.TenantA, enginetest.ProgramA, prohibitingDoc(tc.prohibited...))
			if err != nil {
				t.Fatal(err)
			}
			if err := h.Engine.HandleScopeChanged(h.Ctx(), enginetest.TenantA, enginetest.ProgramA, v2); err != nil {
				t.Fatal(err)
			}
			if st := h.RunToEnd(w, r.ID); st != tc.want {
				t.Fatalf("status %s, want %s; events %v", st, tc.want, enginetest.Types(h.Events(r.ID)))
			}
			if tc.want == engine.StatusPaused && len(h.Exec.CallsTo("echo")) != echoes {
				t.Fatal("the prohibited echo ran after the scope change")
			}
		})
	}
}
