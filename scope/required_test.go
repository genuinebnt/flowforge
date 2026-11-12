package scope

import (
	"context"
	"encoding/json"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Required cases of spec 25.3 (ff-scope). Expected values live in the
// conformance data under conformance/scope/; each test runs the cases whose
// id carries its number and fails if there are none.

func requireNormalize(t *testing.T, id string) {
	t.Helper()
	if runNormalizeCases(t, matchID(id)) == 0 {
		t.Fatalf("no normalize cases with id prefix %s", id)
	}
}

func requireDecide(t *testing.T, id string) {
	t.Helper()
	if runDecideCases(t, matchID(id)) == 0 {
		t.Fatalf("no decide cases with id prefix %s", id)
	}
}

func TestScope001_TrailingDot(t *testing.T) {
	requireNormalize(t, "scope_001")
	requireDecide(t, "scope_001")
}

func TestScope002_PunycodeLookalike(t *testing.T) {
	requireNormalize(t, "scope_002")
	requireDecide(t, "scope_002")
	lookalike, err1 := Normalize("аpple.com", TargetHost)
	genuine, err2 := Normalize("apple.com", TargetHost)
	if err1 != nil || err2 != nil || lookalike.String() == genuine.String() {
		t.Fatalf("lookalike %q (%v) must differ from %q (%v)", lookalike, err1, genuine, err2)
	}
}

func TestScope003_BadNames(t *testing.T) {
	requireNormalize(t, "scope_003")
	requireDecide(t, "scope_003")
}

func TestScope004_Userinfo(t *testing.T) {
	requireNormalize(t, "scope_004")
	requireDecide(t, "scope_004")
}

func TestScope005_HostSuffixTrick(t *testing.T) {
	requireNormalize(t, "scope_005")
	requireDecide(t, "scope_005")
}

func TestScope006_URLCanonical(t *testing.T) {
	requireNormalize(t, "scope_006")
}

func TestScope007_NumericIPv4Forms(t *testing.T) {
	requireNormalize(t, "scope_007")
	requireDecide(t, "scope_007")
}

func TestScope008_MappedIPv6(t *testing.T) {
	requireNormalize(t, "scope_008")
	requireDecide(t, "scope_008")
}

func TestScope009_DenyWins(t *testing.T) {
	requireDecide(t, "scope_009")
}

func TestScope010_ResolvedIPDenied(t *testing.T) {
	requireDecide(t, "scope_010")
}

func TestScope011_PassiveNoAllow(t *testing.T) {
	requireDecide(t, "scope_011")
}

func TestScope012_ClassExceedsProgram(t *testing.T) {
	requireDecide(t, "scope_012")
}

func TestScope013_OutsideWindow(t *testing.T) {
	requireDecide(t, "scope_013")
}

// countingResolver records how often each name is looked up.
type countingResolver struct {
	Resolver
	mu    sync.Mutex
	calls map[string]int
}

func (r *countingResolver) LookupAddrs(ctx context.Context, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[host]++
	r.mu.Unlock()
	return r.Resolver.LookupAddrs(ctx, host)
}

func TestScope014_PinnedIPsDeterministic(t *testing.T) {
	requireDecide(t, "scope_014")

	// The stored decision equals the recomputed one for the same inputs,
	// including resolver answers, and each check resolves exactly once.
	var file decideFile
	for _, f := range loadStrict[decideFile](t, decideSuite) {
		if f.Name == "resolved_ip.json" {
			file = f.Data
		}
	}
	res := &countingResolver{Resolver: fileResolver(t, file)}
	p, err := Compile(file.Scope, WithResolver(res))
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Target: "www.acme.com", Class: core.ClassActive, Now: time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)}
	first := p.Check(context.Background(), req)
	if !first.Allowed() || len(first.PinnedIPs) == 0 {
		t.Fatalf("want an allow with pinned addresses, got %+v", first)
	}
	stored, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	var loaded Decision
	if err := json.Unmarshal(stored, &loaded); err != nil {
		t.Fatal(err)
	}
	again := p.Check(context.Background(), req)
	if !loaded.Equal(again) || !first.Equal(again) {
		t.Fatalf("stored %+v, recomputed %+v", loaded, again)
	}
	recomputed, _ := json.Marshal(again)
	if string(recomputed) != string(stored) {
		t.Fatalf("stored %s, recomputed %s", stored, recomputed)
	}
	if got := res.calls["www.acme.com"]; got != 2 {
		t.Fatalf("two checks made %d lookups, want one each", got)
	}
}

func TestScope015_RedirectHop(t *testing.T) {
	requireDecide(t, "scope_015")
}

func TestScope016_OldVersion(t *testing.T) {
	v1 := Document{ProgramID: "prg_x", Version: 1,
		Rules:    []Rule{{ID: "sub", Effect: EffectAllow, Kind: RuleWildcardDomain, Value: "*.acme.com"}},
		Activity: Activity{MaxClass: core.ClassActive}}
	v2 := v1
	v2.Version = 2
	v2.Rules = append([]Rule{{ID: "legacy", Effect: EffectDeny, Kind: RuleDomain, Value: "legacy.acme.com"}}, v1.Rules...)

	old, err := Compile(v1)
	if err != nil {
		t.Fatal(err)
	}
	current, err := Compile(v2)
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Target: "legacy.acme.com", Class: core.ClassPassive}
	d1, d2 := old.Check(context.Background(), req), current.Check(context.Background(), req)
	if !d1.Allowed() || d1.ScopeVersion != 1 || d1.RuleID != "sub" {
		t.Fatalf("pinned old version: %+v", d1)
	}
	if d2.Allowed() || d2.ScopeVersion != 2 || d2.Reason != ReasonExplicitDeny || d2.RuleID != "legacy" {
		t.Fatalf("current version: %+v", d2)
	}
	b, _ := json.Marshal(d1)
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil || back["scope_version"] != float64(1) {
		t.Fatalf("scope_version missing from %s", b)
	}
}
