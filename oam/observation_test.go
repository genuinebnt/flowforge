package oam_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/testkit"
)

// fragmentObservation returns the spec 5.8 envelope from the merge suite.
func fragmentObservation(t testing.TB) []byte {
	t.Helper()
	var f struct {
		Steps []struct {
			Observation json.RawMessage `json:"observation"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(testkit.ReadFile(t, "oam/merge", "fragment_5_8.json"), &f); err != nil {
		t.Fatal(err)
	}
	return f.Steps[0].Observation
}

// TestOAM006_FragmentRoundTrip checks spec 25.3 oam_006: the spec 5.8
// fragment survives JSON -> model -> JSON with byte-equal canonical JSON,
// both as a whole envelope and as the bare entities/edges fragment.
func TestOAM006_FragmentRoundTrip(t *testing.T) {
	in := fragmentObservation(t)
	for _, mode := range []oam.DecodeMode{oam.Strict, oam.Lenient} {
		t.Run(mode.String(), func(t *testing.T) {
			o, err := oam.DecodeObservation(in, mode)
			if err != nil {
				t.Fatal(err)
			}
			if err := o.Validate(); err != nil {
				t.Fatal(err)
			}
			out, err := oam.EncodeObservation(o)
			if err != nil {
				t.Fatal(err)
			}
			if a, b := mustCanon(t, json.RawMessage(in)), mustCanon(t, json.RawMessage(out)); a != b {
				t.Fatalf("round trip differs:\n in  %s\n out %s", a, b)
			}
		})
	}
	t.Run("fragment", func(t *testing.T) {
		var frag struct {
			Entities []json.RawMessage `json:"entities"`
			Edges    []json.RawMessage `json:"edges"`
		}
		if err := json.Unmarshal(in, &frag); err != nil {
			t.Fatal(err)
		}
		var ents []oam.ObservedEntity
		for _, raw := range frag.Entities {
			var e oam.ObservedEntity
			if err := json.Unmarshal(raw, &e); err != nil {
				t.Fatal(err)
			}
			ents = append(ents, e)
		}
		var edges []oam.ObservedEdge
		for _, raw := range frag.Edges {
			var e oam.ObservedEdge
			if err := json.Unmarshal(raw, &e); err != nil {
				t.Fatal(err)
			}
			edges = append(edges, e)
		}
		want := mustCanon(t, map[string]any{"entities": frag.Entities, "edges": frag.Edges})
		got := mustCanon(t, map[string]any{"entities": ents, "edges": edges})
		if got != want {
			t.Fatalf("fragment differs:\n got  %s\n want %s", got, want)
		}
	})
}

// TestOAM007_UnknownPropertyKeys checks spec 25.3 oam_007: unknown keys
// are rejected on requests (Strict) and ignored on stored data (Lenient).
func TestOAM007_UnknownPropertyKeys(t *testing.T) {
	prop := `{"type":"SimpleProperty","name":"title","value":"Acme","colour":"red"}`
	if _, err := oam.DecodeProperty([]byte(prop), oam.Strict); !errors.Is(err, oam.ErrUnknownField) {
		t.Fatalf("strict property: %v", err)
	}
	p, err := oam.DecodeProperty([]byte(prop), oam.Lenient)
	if err != nil {
		t.Fatalf("lenient property: %v", err)
	}
	if p != (oam.SimpleProperty{Name: "title", Value: "Acme"}) {
		t.Fatalf("lenient property %#v", p)
	}
	env := `{"observation_id":"o","tool":{"name":"t"},"observed_at":"2026-11-12T00:00:00Z","confidence":1,
		"entities":[{"ref":"a","asset":{"type":"FQDN","name":"a.acme.com"}}],
		"tags":[{"target":"a","property":` + prop + `}]}`
	if _, err := oam.DecodeObservation([]byte(env), oam.Strict); !errors.Is(err, oam.ErrUnknownField) {
		t.Fatalf("strict envelope: %v", err)
	}
	var stored oam.Observation
	if err := json.Unmarshal([]byte(env), &stored); err != nil {
		t.Fatalf("stored envelope: %v", err)
	}
	if stored.Tags[0].Property != (oam.SimpleProperty{Name: "title", Value: "Acme"}) {
		t.Fatalf("stored tag %#v", stored.Tags[0])
	}
	// Every level is checked, including nested relation headers, and the
	// match is exact rather than case-insensitive.
	for _, bad := range []string{
		`{"observation_id":"o","tool":{"name":"t","colour":1},"observed_at":"2026-11-12T00:00:00Z","confidence":1}`,
		`{"observation_id":"o","Tool":{"name":"t"},"observed_at":"2026-11-12T00:00:00Z","confidence":1}`,
		`{"observation_id":"o","tool":{"name":"t"},"observed_at":"2026-11-12T00:00:00Z","confidence":1,"entities":[{"ref":"a","REF":"b","asset":{"type":"FQDN","name":"x.y"}}]}`,
		`{"observation_id":"o","tool":{"name":"t"},"observed_at":"2026-11-12T00:00:00Z","confidence":1,"entities":[{"ref":"a","asset":{"type":"FQDN","Name":"x.y"}}]}`,
		`{"observation_id":"o","tool":{"name":"t"},"observed_at":"2026-11-12T00:00:00Z","confidence":1,"edges":[{"from":"a","to":"b","relation":{"type":"BasicDNSRelation","label":"dns_record","header":{"rr_type":1,"class":1,"ttl":1,"x":2}}}]}`,
	} {
		if _, err := oam.DecodeObservation([]byte(bad), oam.Strict); !errors.Is(err, oam.ErrUnknownField) {
			t.Errorf("strict accepted %s: %v", bad, err)
		}
		if _, err := oam.DecodeObservation([]byte(bad), oam.Lenient); err != nil {
			t.Errorf("lenient rejected %s: %v", bad, err)
		}
	}
}

func TestDecodeStrictStructure(t *testing.T) {
	cases := map[string]error{
		`{"type":"FQDN","name":"a.b","name":"c.d"}`: oam.ErrDuplicateField,
		`{"type":"FQDN","name":"a.b"} {}`:           oam.ErrMalformed,
		`{"type":"FQDN","name":7}`:                  oam.ErrMalformed,
		`{"type":"Person","a":1,"a":2}`:             oam.ErrDuplicateField,
		`{"type":7}`:                                oam.ErrInvalidAsset,
		`[]`:                                        oam.ErrMalformed,
		`{"type":"Service","unique_id":"x","attributes":{"server":"nginx"}}`: oam.ErrMalformed,
	}
	for in, want := range cases {
		if _, err := oam.DecodeAsset([]byte(in), oam.Strict); !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", in, err, want)
		}
	}
	if _, err := oam.DecodeRelation([]byte(`{"type":"FancyRelation","label":"x"}`), oam.Lenient); !errors.Is(err, oam.ErrUnknownRelationType) {
		t.Errorf("unknown relation type: %v", err)
	}
	if _, err := oam.DecodeProperty([]byte(`{"type":"FancyProperty","name":"x"}`), oam.Lenient); !errors.Is(err, oam.ErrUnknownPropertyType) {
		t.Errorf("unknown property type: %v", err)
	}
}

func TestObservationValidate(t *testing.T) {
	base := func() oam.Observation {
		return obs("o", "tool", t0, 0.5, ent("a", oam.FQDN{Name: "a.acme.com"}), ent("b", oam.IPAddress{Address: "192.0.2.1"}))
	}
	many := base()
	many.Entities = nil
	for i := range oam.MaxBatchEntities + 1 {
		many.Entities = append(many.Entities, ent(fmt.Sprint("r", i), oam.FQDN{Name: fmt.Sprintf("h%d.acme.com", i)}))
	}
	cases := []struct {
		name string
		mod  func(*oam.Observation)
		want error
	}{
		{"valid", func(*oam.Observation) {}, nil},
		{"missing id", func(o *oam.Observation) { o.ObservationID = "" }, oam.ErrInvalidObservation},
		{"missing tool", func(o *oam.Observation) { o.Tool.Name = " " }, oam.ErrInvalidObservation},
		{"missing time", func(o *oam.Observation) { o.ObservedAt = time.Time{} }, oam.ErrInvalidObservation},
		{"confidence 0 allowed", func(o *oam.Observation) { o.Confidence = 0 }, nil},
		{"confidence > 1", func(o *oam.Observation) { o.Confidence = 1.5 }, oam.ErrInvalidObservation},
		{"confidence NaN", func(o *oam.Observation) { o.Confidence = math.NaN() }, oam.ErrInvalidObservation},
		{"batch too large", func(o *oam.Observation) { *o = many }, oam.ErrBatchTooLarge},
		{"exactly 500", func(o *oam.Observation) { *o = many; o.Entities = o.Entities[:oam.MaxBatchEntities] }, nil},
		{"duplicate ref", func(o *oam.Observation) { o.Entities[1].Ref = "a" }, oam.ErrInvalidObservation},
		{"empty ref", func(o *oam.Observation) { o.Entities[1].Ref = "" }, oam.ErrInvalidObservation},
		{"ref looks like an id", func(o *oam.Observation) {
			o.Entities[1].Ref = oam.EntityID("", "", "FQDN", "fqdn:x")
		}, oam.ErrInvalidObservation},
		{"nil asset", func(o *oam.Observation) { o.Entities[1].Asset = nil }, oam.ErrInvalidAsset},
		{"invalid asset", func(o *oam.Observation) { o.Entities[1].Asset = oam.IPAddress{Address: "300.1.1.1"} }, oam.ErrInvalidAsset},
		{"edge to unknown ref", func(o *oam.Observation) {
			o.Edges = []oam.ObservedEdge{{From: "a", To: "nope", Relation: oam.SimpleRelation{Label: "x"}}}
		}, oam.ErrUnresolvedRef},
		{"edge to entity id", func(o *oam.Observation) {
			o.Edges = []oam.ObservedEdge{{From: "a", To: oam.EntityID("t", "p", "FQDN", "fqdn:z"), Relation: oam.SimpleRelation{Label: "x"}}}
		}, nil},
		{"edge to bare uuid is not an id", func(o *oam.Observation) {
			o.Edges = []oam.ObservedEdge{{From: "a", To: "01234567-89ab-7def-8123-456789abcdef", Relation: oam.SimpleRelation{Label: "x"}}}
		}, oam.ErrUnresolvedRef},
		{"invalid relation", func(o *oam.Observation) {
			o.Edges = []oam.ObservedEdge{{From: "a", To: "b", Relation: oam.PortRelation{Label: "port"}}}
		}, oam.ErrInvalidRelation},
		{"nil relation", func(o *oam.Observation) { o.Edges = []oam.ObservedEdge{{From: "a", To: "b"}} }, oam.ErrInvalidRelation},
		{"label endpoints", func(o *oam.Observation) {
			o.Edges = []oam.ObservedEdge{{From: "a", To: "b", Relation: oam.SimpleRelation{Label: "contains"}}}
		}, oam.ErrLabelEndpoints},
		{"tag on edge id", func(o *oam.Observation) {
			o.Tags = []oam.ObservedTag{{Target: oam.EdgeID("t", "p", "a", "b", "SimpleRelation", "x", ""), Property: oam.SimpleProperty{Name: "n"}}}
		}, nil},
		{"tag unknown target", func(o *oam.Observation) {
			o.Tags = []oam.ObservedTag{{Target: "zz", Property: oam.SimpleProperty{Name: "n"}}}
		}, oam.ErrUnresolvedRef},
		{"flowforge property needs kind", func(o *oam.Observation) {
			o.Tags = []oam.ObservedTag{{Target: "a", Property: oam.FlowforgeProperty{Name: "n", Value: 1}}}
		}, oam.ErrInvalidProperty},
		{"source property range", func(o *oam.Observation) {
			o.Tags = []oam.ObservedTag{{Target: "a", Property: oam.SourceProperty{Name: "s", Confidence: 101}}}
		}, oam.ErrInvalidProperty},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := base()
			c.mod(&o)
			err := o.Validate()
			if c.want == nil {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				return
			}
			if !errors.Is(err, c.want) || !errors.Is(err, oam.ErrInvalidObservation) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			var ve *oam.ValidationError
			if !errors.As(err, &ve) || ve.Path == "" {
				t.Fatalf("no path in %v", err)
			}
		})
	}
	var none oam.Observation
	if err := none.Validate(); !errors.Is(err, oam.ErrInvalidObservation) {
		t.Fatalf("zero observation: %v", err)
	}
	if err := (oam.Observation{ObservationID: "x", Tool: oam.Tool{Name: "t"}}).Validate(); err == nil || !strings.Contains(err.Error(), "observed_at") {
		t.Fatalf("missing observed_at: %v", err)
	}
}
