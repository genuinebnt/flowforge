package scope

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/genuinebnt/flowforge/core"
)

func validDocument() Document {
	return Document{Version: 1,
		Rules: []Rule{
			{ID: "a", Effect: EffectAllow, Kind: RuleWildcardDomain, Value: "*.acme.com"},
			{ID: "b", Effect: EffectDeny, Kind: RuleCIDR, Value: "10.0.0.1/8"},
			{ID: "c", Effect: EffectAllow, Kind: RuleURLPrefix, Value: "https://API.acme.com:443/v2/"},
			{ID: "d", Effect: EffectAllow, Kind: RuleASN, Value: "AS64500"},
			{ID: "e", Effect: EffectAllow, Kind: RuleDomain, Value: "Acme.COM."},
		},
		Activity: Activity{MaxClass: core.ClassActive,
			Windows: []Window{{Days: []string{"mon"}, Start: "00:00", End: "24:00", TZ: "Europe/Berlin"}}},
	}
}

func TestValidate(t *testing.T) {
	if err := validDocument().Validate(); err != nil {
		t.Fatalf("valid document rejected: %v", err)
	}
	rule := func(f func(*Rule)) func(*Document) { return func(d *Document) { f(&d.Rules[0]) } }
	win := func(f func(*Window)) func(*Document) { return func(d *Document) { f(&d.Activity.Windows[0]) } }
	cases := []struct {
		name   string
		mutate func(*Document)
		want   error
	}{
		{"empty id", rule(func(r *Rule) { r.ID = "" }), ErrInvalidRule},
		{"duplicate id", func(d *Document) { d.Rules[1].ID = "a" }, ErrDuplicateRuleID},
		{"no effect", rule(func(r *Rule) { r.Effect = EffectUnspecified }), ErrInvalidRule},
		{"no kind", rule(func(r *Rule) { r.Kind = RuleUnspecified }), ErrInvalidRule},
		{"unknown kind", rule(func(r *Rule) { r.Kind = RuleKind(42) }), ErrInvalidRule},
		{"empty label", rule(func(r *Rule) { r.Kind, r.Value = RuleDomain, "a..b.com" }), ErrInvalidRule},
		{"numeric domain", rule(func(r *Rule) { r.Kind, r.Value = RuleDomain, "10.0.0.1" }), ErrInvalidRule},
		{"wildcard without star", rule(func(r *Rule) { r.Value = "acme.com" }), ErrInvalidRule},
		{"bare star", rule(func(r *Rule) { r.Value = "*." }), ErrInvalidRule},
		{"inner star", rule(func(r *Rule) { r.Value = "*.*.acme.com" }), ErrInvalidRule},
		{"cidr without bits", rule(func(r *Rule) { r.Kind, r.Value = RuleCIDR, "10.0.0.0" }), ErrInvalidRule},
		{"url with userinfo", rule(func(r *Rule) { r.Kind, r.Value = RuleURLPrefix, "https://u@acme.com/" }), ErrInvalidRule},
		{"url with query", rule(func(r *Rule) { r.Kind, r.Value = RuleURLPrefix, "https://acme.com/?a=1" }), ErrInvalidRule},
		{"url with empty query", rule(func(r *Rule) { r.Kind, r.Value = RuleURLPrefix, "https://acme.com/?" }), ErrInvalidRule},
		{"url scheme", rule(func(r *Rule) { r.Kind, r.Value = RuleURLPrefix, "ftp://acme.com/" }), ErrInvalidRule},
		{"asn without prefix", rule(func(r *Rule) { r.Kind, r.Value = RuleASN, "64500" }), ErrInvalidRule},
		{"asn without number", rule(func(r *Rule) { r.Kind, r.Value = RuleASN, "AS" }), ErrInvalidRule},
		{"asn too large", rule(func(r *Rule) { r.Kind, r.Value = RuleASN, "AS4294967296" }), ErrInvalidRule},
		{"negative version", func(d *Document) { d.Version = -1 }, ErrInvalidDocument},
		{"max class out of range", func(d *Document) { d.Activity.MaxClass = 9 }, ErrInvalidActivity},
		{"negative rate", func(d *Document) { d.Activity.Rate.Burst = -1 }, ErrInvalidActivity},
		{"NaN rate", func(d *Document) { d.Activity.Rate.RequestsPerSecond = math.NaN() }, ErrInvalidActivity},
		{"infinite rate", func(d *Document) { d.Activity.Rate.RequestsPerSecond = math.Inf(1) }, ErrInvalidActivity},
		{"no days", win(func(w *Window) { w.Days = nil }), ErrInvalidWindow},
		{"long day name", win(func(w *Window) { w.Days = []string{"monday"} }), ErrInvalidWindow},
		{"upper case day", win(func(w *Window) { w.Days = []string{"Mon"} }), ErrInvalidWindow},
		{"short clock", win(func(w *Window) { w.Start = "9:00" }), ErrInvalidWindow},
		{"24:00 start", win(func(w *Window) { w.Start = "24:00" }), ErrInvalidWindow},
		{"minute 60", win(func(w *Window) { w.End = "10:60" }), ErrInvalidWindow},
		{"start equals end", win(func(w *Window) { w.Start, w.End = "10:00", "10:00" }), ErrInvalidWindow},
		{"empty tz", win(func(w *Window) { w.TZ = "" }), ErrInvalidWindow},
		{"host tz", win(func(w *Window) { w.TZ = "Local" }), ErrInvalidWindow},
		{"unknown tz", win(func(w *Window) { w.TZ = "Mars/Olympus_Mons" }), ErrInvalidWindow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := validDocument()
			d = cloneDocument(d)
			c.mutate(&d)
			err := d.Validate()
			if !errors.Is(err, c.want) || !errors.Is(err, ErrInvalidDocument) {
				t.Fatalf("Validate() = %v, want %v wrapped in ErrInvalidDocument", err, c.want)
			}
			if p, cerr := Compile(d); p != nil || cerr == nil {
				t.Fatalf("Compile accepted an invalid document")
			}
		})
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	d := validDocument()
	d.Rules[0].Value = "bad"
	d.Rules[1].Value = "bad"
	d.Activity.Windows[0].TZ = ""
	err := d.Validate()
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) || len(joined.Unwrap()) != 3 {
		t.Fatalf("want three joined errors, got %v", err)
	}
}

func TestDocumentJSONRoundTrip(t *testing.T) {
	var spec decideFile
	for _, f := range loadStrict[decideFile](t, decideSuite) {
		if f.Name == "rest_example.json" {
			spec = f.Data
		}
	}
	if spec.Scope.ProgramID == "" || spec.Scope.Activity.Rate.Burst != 10 || len(spec.Scope.Rules) != 6 {
		t.Fatalf("spec document not decoded: %+v", spec.Scope)
	}
	b, err := json.Marshal(spec.Scope)
	if err != nil {
		t.Fatal(err)
	}
	var back Document
	if err := core.DecodeStrictBytes(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, spec.Scope) {
		t.Fatalf("round trip changed the document:\n%+v\n%+v", spec.Scope, back)
	}
}

func TestEnumText(t *testing.T) {
	for _, s := range []string{"allow", "deny"} {
		var e RuleEffect
		if err := e.UnmarshalText([]byte(s)); err != nil || e.String() != s {
			t.Fatalf("effect %q: %v %v", s, e, err)
		}
	}
	for _, s := range []string{"domain", "wildcard_domain", "cidr", "url_prefix", "asn"} {
		var k RuleKind
		if err := k.UnmarshalText([]byte(s)); err != nil || k.String() != s {
			t.Fatalf("kind %q: %v %v", s, k, err)
		}
	}
	for s, want := range map[string]TargetKind{"": TargetUnspecified, "host": TargetHost, "hostname": TargetHost, "ip": TargetIP, "url": TargetURL, "cidr": TargetCIDR} {
		var k TargetKind
		if err := k.UnmarshalText([]byte(s)); err != nil || k != want {
			t.Fatalf("target kind %q: %v %v", s, k, err)
		}
	}
	for _, s := range []string{"unknown", "in_scope", "out_of_scope"} {
		var st Status
		if err := st.UnmarshalText([]byte(s)); err != nil || st.String() != s {
			t.Fatalf("status %q: %v %v", s, st, err)
		}
	}
	var e RuleEffect
	if err := json.Unmarshal([]byte(`"maybe"`), &e); !errors.Is(err, ErrInvalidEnum) {
		t.Fatalf("unknown effect accepted: %v", err)
	}
	if _, err := json.Marshal(RuleEffect(0)); err == nil {
		t.Fatal("unspecified effect marshalled")
	}
	if _, err := json.Marshal(Rule{Effect: EffectAllow, Kind: RuleKind(-3)}); err == nil {
		t.Fatal("out of range kind marshalled")
	}
}

func TestPolicyAccessors(t *testing.T) {
	d := validDocument()
	d.ProgramID = "prg_x"
	p, err := Compile(d)
	if err != nil {
		t.Fatal(err)
	}
	got := p.Document()
	got.Rules[0].Value = "changed"
	got.Activity.Windows[0].Days[0] = "sun"
	if p.Document().Rules[0].Value != "*.acme.com" || p.Document().Activity.Windows[0].Days[0] != "mon" {
		t.Fatal("Document() exposes internal state")
	}
	d.Rules[0].Value = "*.other.com"
	if p.Document().Rules[0].Value != "*.acme.com" {
		t.Fatal("Compile kept a reference to the caller's rules")
	}
	if p.Version() != 1 || p.ProgramID() != "prg_x" || p.MaxClass() != core.ClassActive {
		t.Fatalf("accessors: %d %q %s", p.Version(), p.ProgramID(), p.MaxClass())
	}
	if asns := p.ASNs(); len(asns) != 1 || asns[0] != 64500 {
		t.Fatalf("ASNs() = %v", asns)
	}
}
