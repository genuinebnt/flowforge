package scope

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// Conformance data layout (spec 25.9). Files are decoded strictly so a
// misspelt field cannot silently weaken a case.

type normalizeCase struct {
	ID      string     `json:"id"`
	Input   string     `json:"input"`
	Kind    TargetKind `json:"kind"`
	Expect  string     `json:"expect"`
	Invalid bool       `json:"invalid"`
	Note    string     `json:"note"`
}

type decideFile struct {
	Description    string              `json:"description"`
	Scope          Document            `json:"scope"`
	Resolver       map[string][]string `json:"resolver"` // absent: checks do not resolve
	ResolverErrors []string            `json:"resolver_errors"`
	Now            string              `json:"now"`
	Cases          []decideCase        `json:"cases"`
}

type decideCase struct {
	ID         string             `json:"id"`
	Target     string             `json:"target"`
	Kind       TargetKind         `json:"kind"`
	Class      core.ActivityClass `json:"class"`
	Now        string             `json:"now"`
	KillSwitch bool               `json:"kill_switch"`
	Resolve    *bool              `json:"resolve"`
	Hops       []Hop              `json:"hops"`
	Note       string             `json:"note"`
	Expect     decideExpect       `json:"expect"`
}

type decideExpect struct {
	Result       DecisionResult `json:"result"`
	Reason       string         `json:"reason"`
	RuleID       *string        `json:"rule_id"`
	Target       *string        `json:"target"`
	PinnedIPs    *[]string      `json:"pinned_ips"`
	ScopeVersion *int           `json:"scope_version"`
	Checked      *int           `json:"checked"` // decisions made by a chain
}

const (
	normalizeSuite = "scope/normalize"
	decideSuite    = "scope/decide"
)

// loadStrict loads a suite with testkit.LoadCases and re-decodes every
// file with core.DecodeStrictBytes to reject unknown fields.
func loadStrict[T any](t *testing.T, suite string) []testkit.CaseFile[T] {
	t.Helper()
	files := testkit.LoadCases[T](t, suite)
	for i, f := range files {
		var strict T
		if err := core.DecodeStrictBytes(testkit.ReadFile(t, suite, f.Name), &strict); err != nil {
			t.Fatalf("%s/%s: strict decode: %v", suite, f.Name, err)
		}
		files[i].Data = strict
	}
	return files
}

func matchID(prefix string) func(string) bool {
	return func(id string) bool { return prefix == "" || strings.HasPrefix(id, prefix) }
}

// runNormalizeCases runs every normalize case whose id is selected and
// returns how many ran.
func runNormalizeCases(t *testing.T, sel func(string) bool) int {
	t.Helper()
	n := 0
	for _, f := range loadStrict[[]normalizeCase](t, normalizeSuite) {
		for i, c := range f.Data {
			if !sel(c.ID) {
				continue
			}
			n++
			t.Run(f.Name+"/"+caseName(c.ID, i), func(t *testing.T) {
				got, err := Normalize(c.Input, c.Kind)
				if c.Invalid {
					if err == nil {
						t.Fatalf("Normalize(%q, %s) = %q, want invalid", c.Input, c.Kind, got)
					}
					if !errors.Is(err, ErrInvalidTarget) {
						t.Fatalf("error %v does not wrap ErrInvalidTarget", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("Normalize(%q, %s): %v", c.Input, c.Kind, err)
				}
				if got.String() != c.Expect {
					t.Fatalf("Normalize(%q, %s) = %q, want %q", c.Input, c.Kind, got, c.Expect)
				}
				again, err := Normalize(got.String(), c.Kind)
				if err != nil || again.String() != got.String() {
					t.Fatalf("not idempotent: %q -> %q (%v)", got, again, err)
				}
			})
		}
	}
	return n
}

// runDecideCases runs every decide case whose id is selected against both
// the indexed and the linear matcher and returns how many ran.
func runDecideCases(t *testing.T, sel func(string) bool) int {
	t.Helper()
	n := 0
	for _, f := range loadStrict[decideFile](t, decideSuite) {
		file := f.Data
		res := fileResolver(t, file)
		indexed, err := Compile(file.Scope, WithResolver(res))
		if err != nil {
			t.Fatalf("%s: compile: %v", f.Name, err)
		}
		linear, err := Compile(file.Scope, WithResolver(res), WithMatcher(MatcherLinear))
		if err != nil {
			t.Fatalf("%s: compile linear: %v", f.Name, err)
		}
		for i, c := range file.Cases {
			if !sel(c.ID) {
				continue
			}
			n++
			t.Run(f.Name+"/"+caseName(c.ID, i), func(t *testing.T) {
				req := Request{Target: c.Target, Kind: c.Kind, Class: c.Class, KillSwitch: c.KillSwitch}
				req.Now = parseNow(t, c.Now, file.Now)
				req.SkipResolution = file.Resolver == nil
				if c.Resolve != nil {
					req.SkipResolution = !*c.Resolve
				}
				got, checked := runCase(indexed, req, c.Hops)
				ref, refChecked := runCase(linear, req, c.Hops)
				if !got.Equal(ref) || checked != refChecked {
					t.Fatalf("indexed and linear matchers disagree:\nindexed %+v\nlinear  %+v", got, ref)
				}
				checkExpect(t, c, file.Scope.Version, req.Now, got, checked)
			})
		}
	}
	return n
}

func runCase(p *Policy, req Request, hops []Hop) (Decision, int) {
	if hops == nil {
		return p.Check(context.Background(), req), 1
	}
	res := p.CheckChain(context.Background(), req, hops)
	if res.Allowed != (res.Final().Allowed() && len(res.Decisions) == len(hops)+1) {
		return Decision{Reason: "inconsistent ChainResult.Allowed"}, len(res.Decisions)
	}
	return res.Final(), len(res.Decisions)
}

func checkExpect(t *testing.T, c decideCase, version int, now time.Time, got Decision, checked int) {
	t.Helper()
	e := c.Expect
	if got.Result != e.Result {
		t.Errorf("result = %s (%s), want %s", got.Result, got.Reason, e.Result)
	}
	if e.Reason != "" && got.Reason != e.Reason {
		t.Errorf("reason = %q, want %q", got.Reason, e.Reason)
	}
	if e.RuleID != nil && got.RuleID != *e.RuleID {
		t.Errorf("rule_id = %q, want %q", got.RuleID, *e.RuleID)
	}
	if e.Target != nil && got.Target != *e.Target {
		t.Errorf("normalized target = %q, want %q", got.Target, *e.Target)
	}
	if e.PinnedIPs != nil {
		if want := mustAddrs(t, *e.PinnedIPs); !slices.Equal(got.PinnedIPs, want) {
			t.Errorf("pinned_ips = %v, want %v", got.PinnedIPs, want)
		}
	}
	wantVersion := version
	if e.ScopeVersion != nil {
		wantVersion = *e.ScopeVersion
	}
	if got.ScopeVersion != wantVersion {
		t.Errorf("scope_version = %d, want %d", got.ScopeVersion, wantVersion)
	}
	if e.Checked != nil && checked != *e.Checked {
		t.Errorf("chain made %d decisions, want %d", checked, *e.Checked)
	}
	if !got.DecidedAt.Equal(now) {
		t.Errorf("decided_at = %v, want the request time %v", got.DecidedAt, now)
	}
	checkDecisionInvariants(t, got)
}

// checkDecisionInvariants holds for every decision Check makes.
func checkDecisionInvariants(t testing.TB, d Decision) {
	t.Helper()
	withRule := d.Reason == ReasonAllow || d.Reason == ReasonExplicitDeny || d.Reason == ReasonResolvedIPDenied
	if withRule != (d.RuleID != "") {
		t.Errorf("reason %q with rule_id %q", d.Reason, d.RuleID)
	}
	if (d.Result == ResultAllow) != (d.Reason == ReasonAllow) {
		t.Errorf("result %s with reason %q", d.Result, d.Reason)
	}
	if d.Result != ResultAllow && len(d.PinnedIPs) > 0 {
		t.Errorf("deny carries pinned addresses %v", d.PinnedIPs)
	}
	if d.PinnedIPs == nil {
		t.Errorf("pinned_ips is nil")
	}
}

func fileResolver(t *testing.T, f decideFile) StaticResolver {
	t.Helper()
	r := StaticResolver{Answers: map[string][]netip.Addr{}, Errors: map[string]error{}}
	for host, addrs := range f.Resolver {
		r.Answers[host] = mustAddrs(t, addrs)
	}
	for _, host := range f.ResolverErrors {
		r.Errors[host] = errors.New("conformance: injected resolver failure")
	}
	return r
}

func mustAddrs(t testing.TB, ss []string) []netip.Addr {
	t.Helper()
	out := make([]netip.Addr, 0, len(ss))
	for _, s := range ss {
		a, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("bad address %q in conformance data: %v", s, err)
		}
		out = append(out, a)
	}
	return out
}

func parseNow(t *testing.T, values ...string) time.Time {
	t.Helper()
	for _, v := range values {
		if v == "" {
			continue
		}
		ts, err := core.ParseTime(v)
		if err != nil {
			t.Fatalf("bad time %q: %v", v, err)
		}
		return ts
	}
	return time.Time{}
}

func caseName(id string, i int) string {
	if id != "" {
		return id
	}
	return fmt.Sprintf("case%02d", i)
}

func TestScopeConformance_Normalize(t *testing.T) {
	if n := runNormalizeCases(t, matchID("")); n == 0 {
		t.Fatal("no normalize cases")
	}
}

func TestScopeConformance_Decide(t *testing.T) {
	if n := runDecideCases(t, matchID("")); n == 0 {
		t.Fatal("no decide cases")
	}
}
