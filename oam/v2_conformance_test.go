package oam_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/testkit"
)

// urlTemplateFile is the schema of conformance/oam/url_template/*.json.
type urlTemplateFile struct {
	Description string `json:"description"`
	Cases       []struct {
		Name     string   `json:"name"`
		Inputs   []string `json:"inputs"`
		Template string   `json:"template"`
		Error    bool     `json:"error"`
	} `json:"cases"`
}

func urlTemplateCases(t *testing.T) []string {
	var all []string
	for _, f := range testkit.LoadCases[urlTemplateFile](t, "oam/url_template") {
		for _, c := range f.Data.Cases {
			all = append(all, c.Inputs...)
			if c.Template != "" {
				all = append(all, c.Template)
			}
		}
	}
	return all
}

// TestOAMConformance_URLTemplate runs conformance/oam/url_template: every
// input of a case normalizes to its template, or every input is
// rejected with ErrInvalidURLTemplate.
func TestOAMConformance_URLTemplate(t *testing.T) {
	for _, f := range testkit.LoadCases[urlTemplateFile](t, "oam/url_template") {
		for _, c := range f.Data.Cases {
			t.Run(f.Name+"/"+c.Name, func(t *testing.T) {
				if len(c.Inputs) == 0 || c.Error == (c.Template != "") {
					t.Fatal("case needs inputs and exactly one of template or error")
				}
				for _, in := range c.Inputs {
					got, err := oam.NormalizeURLTemplate(in)
					if c.Error {
						if !errors.Is(err, oam.ErrInvalidURLTemplate) {
							t.Errorf("%q: got %q, %v; want ErrInvalidURLTemplate", in, got, err)
						}
						continue
					}
					if err != nil || got != c.Template {
						t.Errorf("%q: got %q, %v; want %q", in, got, err, c.Template)
					}
				}
			})
		}
	}
}

// labelsFile is the schema of conformance/oam/labels/*.json.
type labelsFile struct {
	Description string `json:"description"`
	Cases       []struct {
		Name     string          `json:"name"`
		Relation json.RawMessage `json:"relation"`
		From     string          `json:"from"`
		To       string          `json:"to"`
		OK       bool            `json:"ok"`
		Error    string          `json:"error"` // default label_endpoints when !ok
	} `json:"cases"`
}

// TestOAMConformance_Labels runs conformance/oam/labels: the v2 relation
// labels' endpoint rules (V2 §3.2 rows of the 5.5 table), checked both
// by ValidateEdge and through a whole observation.
func TestOAMConformance_Labels(t *testing.T) {
	for _, f := range testkit.LoadCases[labelsFile](t, "oam/labels") {
		for _, c := range f.Data.Cases {
			t.Run(f.Name+"/"+c.Name, func(t *testing.T) {
				want := sentinels[c.Error]
				if c.Error == "" {
					want = oam.ErrLabelEndpoints
				}
				if c.Error == "invalid_relation" {
					want = oam.ErrInvalidRelation
				}
				rel, err := oam.DecodeRelation(c.Relation, oam.Strict)
				if err == nil {
					var n oam.Relation
					if n, err = rel.Normalize(); err == nil {
						err = oam.ValidateEdge(n, c.From, c.To)
					}
				}
				if c.OK != (err == nil) || (!c.OK && !errors.Is(err, want)) {
					t.Fatalf("ValidateEdge: %v, want ok=%v (%v)", err, c.OK, want)
				}
				// The same rule applies inside an observation.
				o := obs("lbl", "x", t0, 0.5, ent("a", sampleAsset(c.From, "a")), ent("b", sampleAsset(c.To, "b")))
				o.Edges = []oam.ObservedEdge{{From: "a", To: "b", Relation: rel}}
				if rel == nil {
					return
				}
				_, err = oam.NewGraph(oam.Config{}).Apply(o)
				if c.OK != (err == nil) || (!c.OK && !errors.Is(err, want)) {
					t.Fatalf("Apply: %v, want ok=%v", err, c.OK)
				}
			})
		}
	}
}

// sampleAsset returns a valid asset of type typ.
func sampleAsset(typ, tag string) oam.Asset {
	switch typ {
	case oam.TypeURL:
		return oam.URL{URL: "https://acme.com/" + tag}
	case oam.TypeFile:
		return oam.File{URL: "https://acme.com/" + tag + ".js"}
	case oam.TypeEndpoint:
		return oam.Endpoint{Method: "GET", URLTemplate: "https://acme.com/api/" + tag}
	case oam.TypeFQDN:
		return oam.FQDN{Name: tag + ".acme.com"}
	case oam.TypeService:
		return oam.Service{UniqueID: tag}
	case oam.TypeNetblock:
		return oam.Netblock{CIDR: "192.0.2.0/24"}
	case oam.TypeIPAddress:
		return oam.IPAddress{Address: "192.0.2.1"}
	case oam.TypeIPNetRecord:
		return oam.IPNetRecord{Handle: "NET-" + tag}
	case oam.TypeDomainRecord:
		return oam.DomainRecord{Domain: tag + ".acme.com"}
	case oam.TypeAutonomousSystem:
		return oam.AutonomousSystem{Number: 64500}
	case oam.TypeAutnumRecord:
		return oam.AutnumRecord{Number: 64500}
	case oam.TypeTLSCertificate:
		return oam.TLSCertificate{FingerprintSHA256: oam.TLSFingerprint([]byte(tag))}
	}
	return oam.Opaque{Type: typ, Content: map[string]any{"name": tag}}
}

// propertiesFile is the schema of conformance/oam/properties/*.json.
type propertiesFile struct {
	Description string `json:"description"`
	Cases       []struct {
		Name       string          `json:"name"`
		Property   json.RawMessage `json:"property"`
		Error      string          `json:"error"`
		StrictOnly bool            `json:"strict_only"`
		TagName    string          `json:"tag_name"`
	} `json:"cases"`
}

// TestOAMConformance_Properties runs conformance/oam/properties: strict
// and lenient decoding plus validation of the v2 properties and the
// exposed_secret conventions (V2 §3.3).
func TestOAMConformance_Properties(t *testing.T) {
	for _, f := range testkit.LoadCases[propertiesFile](t, "oam/properties") {
		for _, c := range f.Data.Cases {
			t.Run(f.Name+"/"+c.Name, func(t *testing.T) {
				check := func(mode oam.DecodeMode) error {
					p, err := oam.DecodeProperty(c.Property, mode)
					if err != nil {
						return err
					}
					if _, _, _, err := oam.SplitProperty(p); err != nil {
						return err
					}
					if c.TagName != "" && p.PropertyName() != c.TagName {
						t.Errorf("%s: tag name %q, want %q", mode, p.PropertyName(), c.TagName)
					}
					return nil
				}
				strictErr, lenientErr := check(oam.Strict), check(oam.Lenient)
				if c.Error == "" {
					if strictErr != nil || lenientErr != nil {
						t.Fatalf("strict %v, lenient %v; want valid", strictErr, lenientErr)
					}
					return
				}
				want := sentinels[c.Error]
				if c.Error == "invalid_property" {
					want = oam.ErrInvalidProperty
				}
				if !errors.Is(strictErr, want) {
					t.Fatalf("strict: %v, want %s", strictErr, c.Error)
				}
				if c.StrictOnly != (lenientErr == nil) {
					t.Fatalf("lenient: %v (strict_only=%v)", lenientErr, c.StrictOnly)
				}
			})
		}
	}
}

// contentChangeFile is the schema of conformance/oam/content_change/*.json.
type contentChangeFile struct {
	Description string `json:"description"`
	Cases       []struct {
		Name       string                  `json:"name"`
		Patch      oam.ContentChangePatch  `json:"patch"`
		Normalized *oam.ContentChangePatch `json:"normalized"`
		Error      bool                    `json:"error"`
	} `json:"cases"`
}

// TestOAMConformance_ContentChange runs conformance/oam/content_change:
// validation and normalization of content_changed patches.
func TestOAMConformance_ContentChange(t *testing.T) {
	for _, f := range testkit.LoadCases[contentChangeFile](t, "oam/content_change") {
		for _, c := range f.Data.Cases {
			t.Run(f.Name+"/"+c.Name, func(t *testing.T) {
				got, err := c.Patch.Normalize()
				if c.Error {
					if !errors.Is(err, oam.ErrInvalidObservation) {
						t.Fatalf("got %+v, %v; want an error", got, err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if c.Normalized != nil && mustCanon(t, got) != mustCanon(t, *c.Normalized) {
					t.Fatalf("normalized %s, want %s", mustCanon(t, got), mustCanon(t, *c.Normalized))
				}
				again, err := got.Normalize()
				if err != nil || mustCanon(t, again) != mustCanon(t, got) {
					t.Fatalf("normalize not idempotent: %v", err)
				}
			})
		}
	}
}
