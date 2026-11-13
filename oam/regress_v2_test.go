package oam_test

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/testkit"
)

// fieldsFile is the schema of conformance/oam/fields/*.json.
type fieldsFile struct {
	Description string `json:"description"`
	Upstream    struct {
		Repository string `json:"repository"`
		Commit     string `json:"commit"`
	} `json:"upstream"`
	Cases []struct {
		Name      string            `json:"name"`
		Kind      string            `json:"kind"` // asset, relation or property
		Producers []string          `json:"producers"`
		Value     json.RawMessage   `json:"value"`
		Fields    map[string]string `json:"fields"`
	} `json:"cases"`
}

// flatKeys lists the members of a JSON object other than "type", with
// nested objects' members as "outer.inner" (and "outer" itself).
func flatKeys(m map[string]any) []string {
	var out []string
	for k, v := range m {
		if k == "type" {
			continue
		}
		out = append(out, k)
		if sub, ok := v.(map[string]any); ok {
			for sk := range sub {
				out = append(out, k+"."+sk)
			}
		}
	}
	slices.Sort(out)
	return out
}

// TestRegress_G_CORE_02_UpstreamFieldNames: every field spec 5.4-5.6
// tagged verify is pinned to the OAM field it carries
// (conformance/oam/fields): a fully populated value decodes strictly,
// re-encodes with exactly the listed fields and the same values, and the
// renamed fields are only OAM's "type" members.
func TestRegress_G_CORE_02_UpstreamFieldNames(t *testing.T) {
	for _, f := range testkit.LoadCases[fieldsFile](t, "oam/fields") {
		if f.Data.Upstream.Commit == "" {
			t.Fatalf("%s: no upstream commit", f.Name)
		}
		for _, c := range f.Data.Cases {
			t.Run(c.Name, func(t *testing.T) {
				var v any
				var err error
				switch c.Kind {
				case "asset":
					v, err = oam.DecodeAsset(c.Value, oam.Strict)
				case "relation":
					v, err = oam.DecodeRelation(c.Value, oam.Strict)
				case "property":
					v, err = oam.DecodeProperty(c.Value, oam.Strict)
				default:
					t.Fatalf("kind %q", c.Kind)
				}
				if err != nil {
					t.Fatal(err)
				}
				b, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				var got, want map[string]any
				_ = json.Unmarshal(b, &got)
				_ = json.Unmarshal(c.Value, &want)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("round trip\n got %s\nwant %s", b, c.Value)
				}
				if keys, listed := flatKeys(got), slices.Sorted(maps.Keys(c.Fields)); !slices.Equal(keys, listed) {
					t.Fatalf("encoded fields %v, mapped %v", keys, listed)
				}
				for ours, theirs := range c.Fields {
					if theirs != ours && theirs != "type" && theirs != "" {
						t.Errorf("%s carries OAM %q: only OAM's type members are renamed", ours, theirs)
					}
				}
			})
		}
	}
}
