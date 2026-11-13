package htmlparse

import (
	"slices"
	"strings"
	"testing"
)

// TestRegress_DoctypeEmptySystemIDLimitedQuirks: an HTML 4.01
// Transitional/Frameset public identifier with an empty but present
// system identifier selects limited-quirks mode (§13.2.6.4.1: only a
// missing system identifier gives quirks). The parser used to treat ""
// as missing, which put the document in quirks mode and kept a <table>
// inside an open <p>.
func TestRegress_DoctypeEmptySystemIDLimitedQuirks(t *testing.T) {
	cases := []struct {
		doctype string
		want    QuirksMode
	}{
		{`<!DOCTYPE html PUBLIC "-//W3C//DTD HTML 4.01 Transitional//EN" "">`, LimitedQuirks},
		{`<!DOCTYPE html PUBLIC "-//W3C//DTD HTML 4.01 Frameset//EN" "">`, LimitedQuirks},
		{`<!DOCTYPE html PUBLIC "-//W3C//DTD HTML 4.01 Transitional//EN">`, Quirks},
		{`<!DOCTYPE html PUBLIC "-//W3C//DTD HTML 4.01 Transitional//EN" "http://www.w3.org/TR/html4/loose.dtd">`, LimitedQuirks},
	}
	for _, c := range cases {
		doc, _ := Parse([]byte(c.doctype+"<p><table>"), Options{})
		if doc.Mode != c.want {
			t.Errorf("%s: mode %v, want %v", c.doctype, doc.Mode, c.want)
		}
		body := doc.Nodes[doc.DocumentElement()].LastChild
		var kids []string
		for k := range doc.Children(body) {
			kids = append(kids, doc.Nodes[k].Name)
		}
		// Outside quirks mode <table> closes the p and becomes its sibling.
		wantKids := []string{"p", "table"}
		if c.want == Quirks {
			wantKids = []string{"p"}
		}
		if !slices.Equal(kids, wantKids) {
			t.Errorf("%s: body children %v, want %v", c.doctype, kids, wantKids)
		}
	}
}

// TestRegress_EndPAtDepthLimit: "</p>" with no p in button scope inserts
// an empty p and closes it. At the depth limit that p is inserted but not
// opened; closing it anyway used to pop through the button up to an
// outer p, moving later content out of the button. The content must stay
// in the deepest open element, as it does without the limit.
func TestRegress_EndPAtDepthLimit(t *testing.T) {
	in := []byte(strings.Repeat("<div>", 3) + "<p><button></p>x")
	limited, _ := Parse(in, Options{MaxDepth: 7}) // html body div div div p button = 7
	unlimited, _ := Parse(in, Options{})
	parentOfX := func(d *Document) string {
		for id := range d.Descendants(d.Root) {
			if d.Nodes[id].Kind == TextNode && d.Nodes[id].Data == "x" {
				return d.Nodes[d.Nodes[id].Parent].Name
			}
		}
		return ""
	}
	if got, want := parentOfX(limited), parentOfX(unlimited); got != want || want != "button" {
		t.Fatalf("x is in <%s> at the depth limit and in <%s> without it, want <button> both", got, want)
	}
}

// TestRegress_DescendantsTemplateOrder: Descendants visits a template's
// own children before its contents whether or not the template is the
// iteration root (the root case used to visit the contents first).
func TestRegress_DescendantsTemplateOrder(t *testing.T) {
	doc, _ := Parse([]byte("<template><i></i></template>"), Options{})
	var tmpl NodeID = None
	for id := range doc.Elements(doc.Root, HTMLNamespace, "template") {
		tmpl = id
	}
	b := doc.NewElement(HTMLNamespace, "b", nil)
	if err := doc.AppendChild(tmpl, b); err != nil {
		t.Fatal(err)
	}
	names := func(root NodeID) []string {
		var out []string
		for id := range doc.Descendants(root) {
			if doc.Nodes[id].Kind == ElementNode {
				out = append(out, doc.Nodes[id].Name)
			}
		}
		return out
	}
	if got := names(tmpl); !slices.Equal(got, []string{"b", "i"}) {
		t.Fatalf("from the template: %v, want [b i]", got)
	}
	if got := names(doc.Root); !slices.Equal(got, []string{"html", "head", "template", "b", "i", "body"}) {
		t.Fatalf("from the root: %v", got)
	}
}
