package content

import (
	"fmt"
	"testing"
)

func TestScratch(t *testing.T) {
	html := `<!doctype html><html><head><meta name="csrf-token" content="abc123XYZ"><script nonce="r4nd0m">var t = 1731499200123; fetch("/api/x")</script>
<link rel=stylesheet href="/app.3f9a1c2b.css?v=123"></head><body class="b a b" onload="init()"><!-- c -->Hello <b>world</b> at 2026-11-13T10:00:00Z
<form action="/login" method=post><input type=hidden name=authenticity_token value="zzzzzz"><input type=hidden name=x value="1"></form>
<style>a { color : red ; }</style><a href="javascript:void(0)">x</a></body></html>`
	n := Normalize(KindHTML, []byte(html), NormalizeOptions{})
	fmt.Printf("%s\n%+v\n", n.Text, n.Masks)
	for _, d := range n.Derived {
		dn := Normalize(d.Kind, d.Body, NormalizeOptions{})
		fmt.Printf("--- %s %s\n%s", d.Suffix, d.Kind, dn.Text)
	}
	n2 := Normalize(KindHTML, n.Text, NormalizeOptions{})
	fmt.Println("idem:", string(n2.Text) == string(n.Text))
	css := Normalize(KindCSS, []byte("a > b , c{color : red !important;background:url( /x.png?v=1 );}\n@media (max-width: 100px){.x{margin:0}}"), NormalizeOptions{})
	fmt.Printf("%s", css.Text)
	j := Normalize(KindJSON, []byte(`{"b":[1,{"z":null,"a":"x"}],"a":true,"requestId":"abc","ts":1700000000}`), NormalizeOptions{})
	fmt.Printf("%s", j.Text)
	x := Normalize(KindXML, []byte(`<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" b="1" a="2"><url><loc> https://a/ </loc></url></urlset>`), NormalizeOptions{})
	fmt.Printf("%s", x.Text)
	h := Normalize(KindHeaders, HeaderBlock([]Header{{"Date", "x"}, {"Content-Security-Policy", "script-src 'self' 'nonce-abc' https://cdn; default-src 'none'"}, {"Set-Cookie", "sid=secret; Path=/; HttpOnly; Expires=Wed, 21 Oct 2026 07:28:00 GMT; Secure"}}), NormalizeOptions{})
	fmt.Printf("%s", h.Text)
}
