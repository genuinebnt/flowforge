package zzprobe

import (
	"fmt"
	"testing"

	"github.com/genuinebnt/flowforge/oam"
)

func TestProbe(t *testing.T) {
	for _, s := range []string{
		"https://api.acme.com/v1/users/42",
		"HTTPS://API.ACME.COM:443/v1/users/42/",
		"https://api.acme.com/v1/users/${user.id}",
		"https://api.acme.com/v1/users/{userId}/posts/{id}",
		"https://api.acme.com/v1/users/%31%32",
		"https://api.acme.com/files/${name}.json?x=1#frag",
		"https://api.acme.com/a/b/../c/./d",
		"https://api.acme.com/a/${x}/../c",
		"https://api.acme.com",
		"https://api.acme.com?q=1",
		"https://bücher.de/Katalog",
		"http://[2001:DB8::1]:8080/x",
		"https://api.acme.com/obj/550E8400-E29B-41D4-A716-446655440000",
		"https://api.acme.com/blob/deadbeefdeadbeef0123",
		"https://api.acme.com/blob/deadbeef",
		"https://api.acme.com/a%2Fb/%7Bid%7D",
		"https://api.acme.com/a b/{",
		"https://api.acme.com/ffph1z/${x}",
		"https://${host}/x",
		"https://api.acme.com${path}",
		"/api/users",
		"//api.acme.com/x",
		"https://api.acme.com:${port}/x",
		"https://api.acme.com/${a ? `/x/${b}` : '#'}/tail",
		"https://api.acme.com/${unterminated",
		"ftp://acme.com/x",
		"https://user@acme.com/x",
		"https://api.acme.com//double//",
		"https://api.acme.com/~user/%7euser",
		"https://api.acme.com/v2/items/{id}/{uuid}/{hex}/{expr}",
		"  https://api.acme.com/x  ",
		"https://API.acme.com./x",
		"https://api.acme.com/%zz",
		"https://api.acme.com/a?b=${c}/d",
		"https://api.acme.com/0/007/12345678901234567890",
		"https://api.acme.com/v1.2/x",
	} {
		r, err := oam.NormalizeURLTemplate(s)
		r2, err2 := oam.NormalizeURLTemplate(r)
		fmt.Printf("%-60q -> %q %v | idem=%v %v\n", s, r, err, r2 == r || err != nil, err2)
	}
}
