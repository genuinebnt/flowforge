package alerts

import (
	"bytes"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// TestAlrProp_DedupeFastPathCanonical pins the dedupe-key fast path to
// core.CanonicalJSON, byte for byte, on random strings (control bytes,
// invalid UTF-8, U+2028, HTML characters), ints, bools and nils.
func TestAlrProp_DedupeFastPathCanonical(t *testing.T) {
	rng := testkit.Rng(t)
	alphabet := []string{"a", "é", "\"", "\\", "\n", "\r", "\t", "\b", "\f", "\x00", "\x1f", "\x7f", "<", ">", "&", " ", " ", "\xff", "\xe2\x80", "😀", " "}
	for range testkit.Cases() {
		n := core.IntN(rng, 4)
		vals := make([]any, n)
		for i := range vals {
			switch core.IntN(rng, 4) {
			case 0:
				var b bytes.Buffer
				for range core.IntN(rng, 8) {
					b.WriteString(alphabet[core.IntN(rng, len(alphabet))])
				}
				vals[i] = b.String()
			case 1:
				vals[i] = int64(rng.Uint64())
			case 2:
				vals[i] = core.IntN(rng, 2) == 0
			default:
				vals[i] = nil
			}
		}
		fast, ok := appendScalarsJSON(nil, vals)
		if !ok {
			t.Fatalf("fast path refused %#v", vals)
		}
		want, err := core.CanonicalJSON(vals)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(fast, want) {
			t.Fatalf("fast %q != canonical %q for %#v", fast, want, vals)
		}
	}
	if _, ok := appendScalarsJSON(nil, []any{1.5}); ok {
		t.Fatal("floats must take the general path")
	}
}
