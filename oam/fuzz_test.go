package oam_test

import (
	"bytes"
	"testing"

	"github.com/genuinebnt/flowforge/oam"
)

// FuzzOAMDecodeObservation checks that decoding arbitrary bytes never
// panics, and that whatever decodes is stable: decode -> encode -> decode
// -> encode gives identical bytes, and the encoding decodes strictly.
func FuzzOAMDecodeObservation(f *testing.F) {
	f.Add(fragmentObservation(f))
	f.Add([]byte(`{"observation_id":"o","tool":{"name":"t"},"observed_at":"2026-11-12T00:00:00+02:00","confidence":0.5,
		"entities":[{"ref":"a","asset":{"type":"Person","name":"x","n":1e3,"z":[null,{}]}}],
		"tags":[{"target":"a","property":{"type":"FlowforgeProperty","name":"n","value":{"k":[1,2]},"kind":"fact"}}]}`))
	f.Add([]byte(`{"observation_id":"o","tool":{"name":"t"},"observed_at":"2026-11-12T00:00:00Z","confidence":1,
		"edges":[{"from":"a","to":"b","relation":{"type":"SRVDNSRelation","label":"dns_record","header":{"rr_type":33,"class":1,"ttl":5},"priority":1,"weight":2,"port":3}}]}`))
	f.Add([]byte(`{"type":"FQDN"}`))
	f.Add([]byte(`{"entities":[{"asset":{"type":"File","url":"x","FILE_TYPE":"y"}}]}`))
	f.Add([]byte(`[`))
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, mode := range []oam.DecodeMode{oam.Strict, oam.Lenient} {
			o, err := oam.DecodeObservation(data, mode)
			if err != nil {
				continue
			}
			_ = o.Validate()
			g := oam.NewGraph(oam.Config{})
			_, _ = g.Apply(o)
			if _, err := g.CanonicalBytes(); err != nil {
				t.Fatalf("canonical bytes after apply: %v", err)
			}
			b1, err := oam.EncodeObservation(o)
			if err != nil {
				t.Fatalf("decoded observation does not encode: %v", err)
			}
			o2, err := oam.DecodeObservation(b1, oam.Strict)
			if err != nil {
				t.Fatalf("encoding does not decode strictly: %v\n%s", err, b1)
			}
			b2, err := oam.EncodeObservation(o2)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(b1, b2) {
				t.Fatalf("unstable:\n%s\n%s", b1, b2)
			}
		}
	})
}
