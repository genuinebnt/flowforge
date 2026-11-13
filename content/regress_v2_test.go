package content

import "testing"

// TestRegress_SameLineageKeyRefIndependentOfOrder: two bundles with one
// lineage key in one document (main.0123abcd.js and main.89abcdef.js) are
// one script feature, but which URL its Ref kept depended on the order of
// the tags (and, past 12 features, on the internals of Go's unstable sort),
// although sortFeatures promises output independent of discovery order.
// The Ref reaches Observe as the loads_script File asset.
func TestRegress_SameLineageKeyRefIndependentOfOrder(t *testing.T) {
	a := `<script src="/main.0123abcd.js"></script>`
	b := `<script src="/main.89abcdef.js"></script>`
	ref := func(body string) string {
		fs := Extract(ExtractInput{URL: "https://shop.example/", Kind: KindHTML, Body: []byte(body)})
		var refs []string
		for _, f := range fs.Features {
			if f.Kind == FeatScript {
				refs = append(refs, f.Ref)
			}
		}
		if len(refs) != 1 {
			t.Fatalf("want one script feature, got refs %q", refs)
		}
		return refs[0]
	}
	if x, y := ref(a+b), ref(b+a); x != y {
		t.Fatalf("Ref depends on tag order: %q vs %q", x, y)
	}
}
