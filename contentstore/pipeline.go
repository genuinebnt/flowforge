package contentstore

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/genuinebnt/flowforge/content"
)

// Pipeline is the derivation the store applies to a capture: sniffing,
// normalization and extraction (content.Process), named by a stamp that
// every snapshot records (Snapshot.DeriveStamp). When the stamp changes
// (a normalizer, extractor or rule table was upgraded), Rederive re-derives
// the stored snapshots from their raw bodies (V2 §12.2).
type Pipeline struct {
	// Process derives a capture (nil: content.Process).
	Process func(*content.Capture, content.ProcessOptions) (*content.Processed, error)
	// Stamp identifies the derivation ("" means DerivationStamp()). Two
	// pipelines with one stamp must derive identical records.
	Stamp string
}

func (p Pipeline) withDefaults() Pipeline {
	if p.Process == nil {
		p.Process = content.Process
	}
	if p.Stamp == "" {
		p.Stamp = DerivationStamp()
	}
	return p
}

// DerivationStamp is the stamp of content.Process as compiled:
// "content/" + the hex SHA-256 over lines joined by "\n": one per
// normalizer ("normalizer <name>@<version> <rule table sha256>",
// content.Normalizers order), then one per extractor
// ("extractor <name>@<version>", content.Extractors order).
// Bumping any normalizer or extractor version, or changing a rule table,
// changes it.
func DerivationStamp() string {
	var lines []string
	for _, n := range content.Normalizers() {
		lines = append(lines, "normalizer "+n.ID()+" "+n.RuleTableSHA256)
	}
	for _, e := range content.Extractors() {
		lines = append(lines, "extractor "+e.ID())
	}
	h := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return "content/" + hex.EncodeToString(h[:])
}
