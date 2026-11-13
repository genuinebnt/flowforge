package oam

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// CategoryExposedSecret is the VulnProperty category of a leaked
// credential (V2 design §3.3). Such a tag holds only:
//
//   - id:   "secret:<rule_id>:<keyed_hash>", where keyed_hash is the
//     lower-case hex HMAC-SHA256 of the secret under a per-tenant key
//     (SecretKeyedHash); it deduplicates without revealing the secret;
//   - enum: the rule id;
//   - desc: the redacted preview (SecretPreview): the first 4 and last 2
//     characters around "...", or just "..." for a secret shorter than
//     MinPreviewSecretLen or not printable ASCII;
//   - source: the detector (optional, an identifier such as "ff-secrets@1");
//   - ref:  the content-store finding reference (optional, "finding:<id>").
//
// The secret itself never enters the graph.
const CategoryExposedSecret = "exposed_secret"

// MinPreviewSecretLen is the shortest secret whose preview shows any of
// its characters; shorter secrets would be mostly revealed by 6
// characters.
const MinPreviewSecretLen = 12

// ErrSecretValue means a property value looks like a full secret
// (LooksLikeSecret). It wraps ErrInvalidProperty.
var ErrSecretValue = fmt.Errorf("%w: value looks like a secret", ErrInvalidProperty)

// SecretPreview returns the redacted preview of a secret.
func SecretPreview(secret string) string {
	if len(secret) < MinPreviewSecretLen {
		return "..."
	}
	for i := 0; i < len(secret); i++ {
		if secret[i] < 0x21 || secret[i] > 0x7e {
			return "..."
		}
	}
	return secret[:4] + "..." + secret[len(secret)-2:]
}

// SecretKeyedHash returns the lower-case hex HMAC-SHA256 of secret under
// key (the per-tenant deduplication key).
func SecretKeyedHash(key []byte, secret string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(secret))
	return hex.EncodeToString(m.Sum(nil))
}

// NewExposedSecret builds the VulnProperty of a secret found by ruleID.
// The secret is used only for the preview and the keyed hash.
func NewExposedSecret(ruleID, secret string, key []byte, source, ref string) (VulnProperty, error) {
	if len(key) == 0 {
		return VulnProperty{}, fmt.Errorf("%w: exposed secret needs a keyed-hash key", ErrInvalidProperty)
	}
	p := VulnProperty{
		ID: "secret:" + ruleID + ":" + SecretKeyedHash(key, secret), Description: SecretPreview(secret),
		Source: source, Category: CategoryExposedSecret, Enum: ruleID, Ref: ref,
	}
	return p, p.Validate()
}

func validRuleID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || i > 0 && (c == '_' || c == '.' || c == '-')) {
			return false
		}
	}
	return true
}

func validPreview(s string) bool {
	if s == "..." {
		return true
	}
	if len(s) != 9 || s[4:7] != "..." {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// ValidateExposedSecret checks the exposed_secret conventions on p (see
// CategoryExposedSecret) and that no field LooksLikeSecret.
func ValidateExposedSecret(p VulnProperty) error {
	bad := func(field, format string, args ...any) error {
		return propErr(PropertyVuln, field, format, args...)
	}
	rest, ok := strings.CutPrefix(p.ID, "secret:")
	rule, h, ok2 := strings.Cut(rest, ":")
	switch {
	case !ok || !ok2 || !validRuleID(rule) || len(h) != 64 || !isHex(h) || strings.ToLower(h) != h:
		return bad("id", "exposed_secret id must be secret:<rule_id>:<64 lower-case hex>")
	case p.Enum != rule:
		return bad("enum", "exposed_secret enum must be the rule id %q", rule)
	case !validPreview(p.Description):
		return bad("desc", "exposed_secret desc must be a redacted preview (xxxx...yy or ...)")
	case p.Source != "" && (len(p.Source) > 64 || !validNormalizerVersionOrIdent(p.Source)):
		return bad("source", "exposed_secret source must be a detector identifier")
	case p.Ref != "" && (len(p.Ref) > 256 || strings.ContainsAny(p.Ref, " \t") || hasControl(p.Ref)):
		return bad("ref", "exposed_secret ref must be a reference without spaces")
	}
	return CheckNoSecrets(p)
}

func validNormalizerVersionOrIdent(s string) bool {
	return validRuleID(s) || validNormalizerVersion(s)
}

// CheckNoSecrets returns ErrSecretValue when any string field of p
// LooksLikeSecret. The graph enforces it on exposed_secret tags;
// producers deriving properties from captured bytes can apply it to any
// other property.
func CheckNoSecrets(p Property) error {
	_, _, content, err := splitPropertyUnchecked(p)
	if err != nil {
		return err
	}
	var walk func(path string, v any) error
	walk = func(path string, v any) error {
		switch x := v.(type) {
		case string:
			if LooksLikeSecret(x) {
				return fmt.Errorf("%w (%s.%s)", ErrSecretValue, p.PropertyType(), path)
			}
		case []any:
			for i, e := range x {
				if err := walk(fmt.Sprintf("%s[%d]", path, i), e); err != nil {
					return err
				}
			}
		case map[string]any:
			for k, e := range x {
				if err := walk(path+"."+k, e); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for k, v := range content {
		if err := walk(k, v); err != nil {
			return err
		}
	}
	return nil
}

// secretPrefixes are well-known credential prefixes: a token run that
// starts with one and is at least 16 characters long is a secret whatever
// its entropy.
var secretPrefixes = []string{"AKIA", "ASIA", "ghp_", "gho_", "ghs_", "ghu_", "github_pat_", "glpat-", "xoxa-", "xoxb-",
	"xoxp-", "xoxs-", "sk_live_", "rk_live_", "sk-", "AIza", "eyJ"}

// LooksLikeSecret is the graph's heuristic for "this is a full secret
// value", applied to every string field of an exposed_secret tag (and
// available to producers through CheckNoSecrets). It is true when s
//
//  1. contains a PEM private-key header ("-----BEGIN" ... "PRIVATE KEY"), or
//  2. contains a token run (a maximal run of [A-Za-z0-9+/=_.~-]) that
//     a. starts with a well-known credential prefix (secretPrefixes) and
//     is at least 16 characters long, or
//     b. is at least 20 characters long, is not all hex digits and
//     hyphens (digests, UUIDs and keyed hashes are allowed), contains
//     at least one letter and one digit, and has a Shannon entropy of
//     at least 3.5 bits per character.
//
// The thresholds are deliberately conservative: a 20-character random
// base62 token has an entropy near 4.2 bits per character, English
// identifiers rarely contain digits, and hex digests are exempt by rule
// 2b. False negatives (low-entropy secrets) are caught by the content
// service's rule table before anything reaches the graph.
func LooksLikeSecret(s string) bool {
	if strings.Contains(s, "-----BEGIN") && strings.Contains(s, "PRIVATE KEY") {
		return true
	}
	isTok := func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("+/=_.~-", c) >= 0
	}
	for i := 0; i < len(s); {
		if !isTok(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isTok(s[j]) {
			j++
		}
		if secretRun(s[i:j]) {
			return true
		}
		i = j
	}
	return false
}

func secretRun(r string) bool {
	for _, p := range secretPrefixes {
		if strings.HasPrefix(r, p) && len(r) >= 16 {
			return true
		}
	}
	if len(r) < 20 || isHex(strings.ReplaceAll(r, "-", "")) {
		return false
	}
	letter, digit := false, false
	var counts [256]int
	for i := 0; i < len(r); i++ {
		c := r[i]
		counts[c]++
		letter = letter || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		digit = digit || c >= '0' && c <= '9'
	}
	if !letter || !digit {
		return false
	}
	return shannon(counts[:], len(r)) >= 3.5
}

// shannon returns the entropy in bits per symbol of a byte histogram.
func shannon(counts []int, n int) float64 {
	h := 0.0
	for _, c := range counts {
		if c > 0 {
			p := float64(c) / float64(n)
			h -= p * math.Log2(p)
		}
	}
	return h
}

// splitPropertyUnchecked is SplitProperty without Validate (Validate
// itself calls CheckNoSecrets).
func splitPropertyUnchecked(p Property) (string, string, map[string]any, error) {
	if p == nil {
		return "", "", nil, errors.New("oam: nil property")
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", "", nil, err
	}
	m, err := objectOf(b)
	if err != nil {
		return "", "", nil, err
	}
	delete(m, "type")
	return p.PropertyType(), p.PropertyName(), m, nil
}
