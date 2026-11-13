package content

import (
	"bytes"
	"maps"
	"slices"
	"strings"
	"testing"
)

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

// TestRegress_DigitlessHexBuildHash pins FF_SEED=1794593622660195640 of
// CONTENT-INV-02: the volatile variant renamed main.3f9a1c2b.js to
// main.fcbfaeeb.js, an 8-character hex build hash without a digit, which
// html@1 did not mask (isHashLike wanted a digit in an all-hex part), so
// the normalized page and the script feature changed although only the
// build hash did.
func TestRegress_DigitlessHexBuildHash(t *testing.T) {
	page := string(site(t, "spa", "v1.index.html"))
	if !strings.Contains(page, "main.3f9a1c2b.js") {
		t.Fatal("fixture changed")
	}
	variant := strings.Replace(page, "main.3f9a1c2b.js", "main.fcbfaeeb.js", 1)
	base := Normalize(KindHTML, []byte(page), NormalizeOptions{})
	got := Normalize(KindHTML, []byte(variant), NormalizeOptions{})
	if !bytes.Equal(got.Text, base.Text) {
		t.Fatalf("digitless hash changed the normalized page\n%s", firstDiff(base.Text, got.Text))
	}
	in := func(b string) []string {
		return Extract(ExtractInput{URL: "https://shop.example/", Kind: KindHTML, Body: []byte(b)}).FeatureIDs()
	}
	if !equalIDs(in(variant), in(page)) {
		t.Fatal("digitless hash changed the features")
	}
	css := Normalize(KindCSS, []byte("a{background:url(/img/logo.abcdefab.png)}"), NormalizeOptions{})
	if !bytes.Contains(css.Text, []byte("logo."+hashPlaceholder+".png")) {
		t.Fatalf("css url not masked: %s", css.Text)
	}
	for u, want := range map[string]string{
		"https://shop.example/static/js/main.fcbfaeeb.js":  "https://shop.example/static/js/main.__FF_HASH__.js",
		"https://shop.example/static/js/fcbfaeeb.js":       "https://shop.example/static/js/__FF_HASH__.js",
		"https://shop.example/static/js/chunk-deadbeef.js": "https://shop.example/static/js/chunk-__FF_HASH__.js",
		// Not hash-like: too short, or letters only beyond a-f.
		"https://shop.example/static/js/main.facade.js":   "https://shop.example/static/js/main.facade.js",
		"https://shop.example/static/js/main.feedback.js": "https://shop.example/static/js/main.feedback.js",
		"https://shop.example/static/js/vendor.bundle.js": "https://shop.example/static/js/vendor.bundle.js",
	} {
		if got := LineageKey(u); got != want {
			t.Errorf("LineageKey(%s) = %s, want %s", u, got, want)
		}
	}
}

// TestRegress_FormSubmitterOverrides: WHATWG HTML §4.10.21.3 and
// §4.10.18.6. A submit button's formmethod overrides the form's method (a
// formmethod=get button in a POST form submits GET, with query
// parameters), formenctype overrides the enctype, formmethod and
// formenctype apply without a formaction, a dialog form submits through a
// button whose formmethod is post, and formaction on anything but a submit
// button (a text input, a reset or plain button) is ignored. html.forms@1
// kept POST for formmethod=get, ignored formmethod and formenctype without
// formaction, and took formaction from any button or input.
func TestRegress_FormSubmitterOverrides(t *testing.T) {
	forms := func(body string) map[string][]string {
		fs := Extract(ExtractInput{URL: "https://shop.example/p", Kind: KindHTML, Body: []byte(body)})
		out := map[string][]string{}
		for _, f := range fs.Features {
			switch f.Kind {
			case FeatForm:
				out[f.ID()] = append(out[f.ID()], "enctype="+f.Attr("enctype"))
			case FeatParameter:
				out[f.ID()] = append(out[f.ID()], "")
			}
		}
		return out
	}
	cases := []struct {
		name, body string
		want       map[string][]string
	}{
		{"formmethod get in a POST form", `<form method="post" action="/search"><input name="q">` +
			`<button formmethod="get">Search</button></form>`, map[string][]string{
			"form|ep:POST https://shop.example/search":             {"enctype="},
			"parameter|ep:POST https://shop.example/search body:q": {""},
			"form|ep:GET https://shop.example/search":              {"enctype="},
			"parameter|ep:GET https://shop.example/search query:q": {""},
		}},
		{"formenctype and formmethod without formaction", `<form action="/up"><input name="f" type="file">` +
			`<input type="submit" formmethod="POST" formenctype="multipart/form-data"></form>`, map[string][]string{
			"form|ep:GET https://shop.example/up":              {"enctype="},
			"parameter|ep:GET https://shop.example/up query:f": {""},
			"form|ep:POST https://shop.example/up":             {"enctype=multipart/form-data"},
			"parameter|ep:POST https://shop.example/up body:f": {""},
		}},
		{"formenctype alone", `<form method="post" action="/up"><input name="f">` +
			`<button type="submit" formenctype="text/plain">x</button></form>`, map[string][]string{
			"form|ep:POST https://shop.example/up":             {"enctype=", "enctype=text/plain"},
			"parameter|ep:POST https://shop.example/up body:f": {""},
		}},
		{"dialog form with a post button", `<form method="dialog" action="/confirm"><input name="ok">` +
			`<button formmethod="post">OK</button><button>Close</button></form>`, map[string][]string{
			"form|ep:POST https://shop.example/confirm":              {"enctype="},
			"parameter|ep:POST https://shop.example/confirm body:ok": {""},
		}},
		{"formaction on non-submitters ignored", `<form method="post" action="/a"><input name="x" formaction="/b">` +
			`<button type="reset" formaction="/c">r</button><button type="button" formaction="/d">b</button>` +
			`<button commandfor="dlg" command="show-modal" formaction="/e">m</button></form>`, map[string][]string{
			"form|ep:POST https://shop.example/a":             {"enctype="},
			"parameter|ep:POST https://shop.example/a body:x": {""},
		}},
		{"image submitter and empty formaction", `<form method="post" action="/a"><input name="x">` +
			`<input type="image" src="/go.png" formaction="" formmethod="get"></form>`, map[string][]string{
			"form|ep:POST https://shop.example/a":             {"enctype="},
			"parameter|ep:POST https://shop.example/a body:x": {""},
			"form|ep:GET https://shop.example/p":              {"enctype="},
			"parameter|ep:GET https://shop.example/p query:x": {""},
		}},
		{"override equal to the form adds nothing", `<form method="post" action="/a" enctype="multipart/form-data"><input name="x">` +
			`<button formaction="/a" formmethod="post" formenctype="MULTIPART/form-data">s</button></form>`, map[string][]string{
			"form|ep:POST https://shop.example/a":             {"enctype=multipart/form-data"},
			"parameter|ep:POST https://shop.example/a body:x": {""},
		}},
	}
	for _, c := range cases {
		got := forms(c.body)
		for k := range got {
			slices.Sort(got[k])
		}
		if !maps.EqualFunc(got, c.want, slices.Equal) {
			t.Errorf("%s:\n got %v\nwant %v", c.name, got, c.want)
		}
	}
}

// TestContentSlackWebhookShape pins slack_webhook's shape (spec 33.7):
// the webhook prefix and any three '/'-separated alphanumeric segments,
// not only the issued T…/B…/24-character form; two segments are not a
// webhook.
func TestContentSlackWebhookShape(t *testing.T) {
	const p = "https://hooks.slack.com/services/"
	for in, want := range map[string]string{
		"x " + p + "T01ABCDEF/B02GHIJKL/abcdefghijklmnopqrstuvwx y": p + "T01ABCDEF/B02GHIJKL/abcdefghijklmnopqrstuvwx",
		"x " + p + "T0123456789/B0123456789/short y":                p + "T0123456789/B0123456789/short",
		"x " + p + "X1/Y2/z3 y":                                     p + "X1/Y2/z3",
		"x " + p + "T01ABCDEF/B02GHIJKL y":                          "",
	} {
		got := ""
		for _, f := range ScanSecrets([]byte(in), "") {
			if f.RuleID == "slack_webhook" {
				got = in[f.Start:f.End]
			}
		}
		if got != want {
			t.Errorf("%q: matched %q, want %q", in, got, want)
		}
	}
}
