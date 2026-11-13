package content

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/oam"
)

// SecretTier is the confidence tier of a secret rule (V2 §3.3 amended):
// named, structured rules are high; bare entropy is low and never a
// finding on its own.
type SecretTier string

// Secret tiers.
const (
	TierHigh SecretTier = "high"
	TierLow  SecretTier = "low"
)

// SecretFinding is one credential-shaped string found in a document.
type SecretFinding struct {
	RuleID string     `json:"rule_id"`
	Tier   SecretTier `json:"tier"`
	// Start and End are the byte span of the secret in the scanned text.
	Start int `json:"start"`
	End   int `json:"end"`
	// Preview is oam.SecretPreview of the secret (first 4 and last 2
	// characters).
	Preview string `json:"preview"`
	// Doc is the derived-document suffix the secret was found in ("" for
	// the artifact itself).
	Doc string `json:"doc,omitempty"`
	// Secret is the keyed-hash input. It is never serialized, logged or
	// put in a feature; the caller hashes it with the tenant key
	// (KeyedHasher) and stores it only encrypted (V2 §6.4).
	Secret []byte `json:"-"`
}

// KeyedHasher computes the deduplication hash of a secret: lower-case hex
// HMAC-SHA256 under the per-tenant key (oam.SecretKeyedHash). The content
// package never holds tenant keys; the caller supplies the hasher.
type KeyedHasher interface {
	KeyedHash(secret []byte) string
}

// TenantKeyHasher is the HMAC-SHA256 KeyedHasher over a tenant key.
type TenantKeyHasher struct{ Key []byte }

// KeyedHash implements KeyedHasher.
func (h TenantKeyHasher) KeyedHash(secret []byte) string {
	return oam.SecretKeyedHash(h.Key, string(secret))
}

// secretRule is one named rule. scan returns the spans of matches in s.
type secretRule struct {
	id   string
	tier SecretTier
	desc string
	scan func(s string) [][2]int
}

// SecretRuleIDs lists the rule ids with their tier and description, in
// table order (the README's table is checked against it).
func SecretRuleIDs() [][3]string {
	var out [][3]string
	for _, r := range secretRules {
		out = append(out, [3]string{r.id, string(r.tier), r.desc})
	}
	return out
}

// secretRules is the secrets@1 table. Rules are prefix or keyword scans
// followed by an exact shape check (no regular expressions, so the Rust
// port reproduces the spans exactly).
var secretRules = []secretRule{
	{"aws_access_key_id", TierHigh, "AKIA|ASIA|ABIA|ACCA + 16 [A-Z0-9]", prefixScan([]string{"AKIA", "ASIA", "ABIA", "ACCA"}, 16, 16, upperDigit)},
	{"aws_secret_access_key", TierHigh, "aws_secret_access_key / aws_secret_key / secretaccesskey = 40 [A-Za-z0-9/+=]",
		assignScan([]string{"aws_secret_access_key", "aws_secret_key", "secretaccesskey", "aws_secret"}, 40, 40, base64Char)},
	{"github_token", TierHigh, "ghp_|gho_|ghu_|ghs_|ghr_ + 36 [A-Za-z0-9]", prefixScan([]string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"}, 36, 36, alnum)},
	{"github_fine_grained_pat", TierHigh, "github_pat_ + 82 [A-Za-z0-9_]", prefixScan([]string{"github_pat_"}, 82, 82, alnumUnderscore)},
	{"gitlab_token", TierHigh, "glpat- + 20 [A-Za-z0-9_-]", prefixScan([]string{"glpat-"}, 20, 20, alnumDashUnderscore)},
	{"slack_token", TierHigh, "xox[abeprs]- + 20..255 [A-Za-z0-9-]", prefixScan([]string{"xoxa-", "xoxb-", "xoxe-", "xoxp-", "xoxr-", "xoxs-"}, 20, 255, alnumDash)},
	{"slack_webhook", TierHigh, "https://hooks.slack.com/services/T…/B…/[A-Za-z0-9]{24}", slackWebhookScan},
	{"stripe_secret_key", TierHigh, "sk_live_|rk_live_ + 24..99 [A-Za-z0-9]", prefixScan([]string{"sk_live_", "rk_live_"}, 24, 99, alnum)},
	{"google_api_key", TierHigh, "AIza + 35 [A-Za-z0-9_-]", prefixScan([]string{"AIza"}, 35, 35, alnumDashUnderscore)},
	{"openai_api_key", TierHigh, "sk-proj- + 40..200 [A-Za-z0-9_-]", prefixScan([]string{"sk-proj-"}, 40, 200, alnumDashUnderscore)},
	{"anthropic_api_key", TierHigh, "sk-ant- + 40..200 [A-Za-z0-9_-]", prefixScan([]string{"sk-ant-"}, 40, 200, alnumDashUnderscore)},
	{"sendgrid_api_key", TierHigh, "SG. + 22 [A-Za-z0-9_-] + . + 43 [A-Za-z0-9_-]", sendgridScan},
	{"npm_token", TierHigh, "npm_ + 36 [A-Za-z0-9]", prefixScan([]string{"npm_"}, 36, 36, alnum)},
	{"azure_storage_key", TierHigh, "AccountKey= + 86..88 base64", assignScan([]string{"accountkey"}, 86, 88, base64Char)},
	{"jwt", TierHigh, "eyJ….eyJ….sig (header decodes to JSON with \"alg\")", jwtScan},
	{"private_key", TierHigh, "-----BEGIN [RSA |EC |DSA |OPENSSH |ENCRYPTED |PGP ]PRIVATE KEY[ BLOCK]----- … END", privateKeyScan},
	{"connection_string", TierHigh, "postgres|mysql|mongodb[+srv]|redis[s]|amqp[s]|mssql|sqlserver://user:PASSWORD@", connStringScan(connSchemes)},
	{"url_credentials", TierHigh, "http[s]|ftp://user:PASSWORD@", connStringScan([]string{"http", "https", "ftp"})},
	{"generic_high_entropy", TierLow, "secret-like key (api_key, secret, token, password, …) = quoted 16..128 chars, entropy >= 3.5, letter and digit", genericScan},
}

func upperDigit(c byte) bool { return c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' }
func alnum(c byte) bool      { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' }
func alnumUnderscore(c byte) bool {
	return alnum(c) || c == '_'
}
func alnumDash(c byte) bool           { return alnum(c) || c == '-' }
func alnumDashUnderscore(c byte) bool { return alnum(c) || c == '-' || c == '_' }
func base64Char(c byte) bool          { return alnum(c) || c == '+' || c == '/' || c == '=' }
func base64URLChar(c byte) bool       { return alnum(c) || c == '-' || c == '_' }

// boundary reports that the byte before i (if any) cannot continue a
// token, so "XAKIA..." is not an AWS key.
func boundaryBefore(s string, i int) bool { return i == 0 || !alnumUnderscore(s[i-1]) }

// prefixScan matches prefix + between lo and hi body characters, with a
// token boundary on both sides.
func prefixScan(prefixes []string, lo, hi int, body func(byte) bool) func(string) [][2]int {
	return func(s string) [][2]int {
		var out [][2]int
		for _, p := range prefixes {
			for off := 0; ; {
				i := strings.Index(s[off:], p)
				if i < 0 {
					break
				}
				i += off
				off = i + 1
				if !boundaryBefore(s, i) {
					continue
				}
				j := i + len(p)
				for j < len(s) && j-i-len(p) < hi && body(s[j]) {
					j++
				}
				n := j - i - len(p)
				if n < lo || j < len(s) && body(s[j]) {
					continue
				}
				out = append(out, [2]int{i, j})
			}
		}
		return out
	}
}

// assignScan matches a keyword (case-insensitive, whole identifier) then
// optional quote, white space, ':' or '=', white space and optional
// quote, then a value of lo..hi body characters.
func assignScan(keys []string, lo, hi int, body func(byte) bool) func(string) [][2]int {
	return func(s string) [][2]int {
		var out [][2]int
		ls := strings.ToLower(s)
		for _, k := range keys {
			for off := 0; ; {
				i := strings.Index(ls[off:], k)
				if i < 0 {
					break
				}
				i += off
				off = i + 1
				end := i + len(k)
				if !boundaryBefore(s, i) || end < len(s) && alnumUnderscore(s[end]) {
					continue
				}
				if st, en, ok := assignedValue(s, end, lo, hi, body); ok {
					out = append(out, [2]int{st, en})
				}
			}
		}
		return out
	}
}

// assignedValue parses `["']? \s* [:=] \s* ["']? VALUE` from j.
func assignedValue(s string, j, lo, hi int, body func(byte) bool) (int, int, bool) {
	if j < len(s) && (s[j] == '"' || s[j] == '\'') {
		j++
	}
	for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
		j++
	}
	if j >= len(s) || s[j] != ':' && s[j] != '=' {
		return 0, 0, false
	}
	j++
	for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
		j++
	}
	if j < len(s) && (s[j] == '"' || s[j] == '\'' || s[j] == '`') {
		j++
	}
	st := j
	for j < len(s) && j-st < hi && body(s[j]) {
		j++
	}
	if j-st < lo || j < len(s) && body(s[j]) {
		return 0, 0, false
	}
	return st, j, true
}

func slackWebhookScan(s string) [][2]int {
	const p = "https://hooks.slack.com/services/"
	var out [][2]int
	for off := 0; ; {
		i := strings.Index(s[off:], p)
		if i < 0 {
			return out
		}
		i += off
		off = i + 1
		j := i + len(p)
		segs := 0
		for segs < 3 {
			st := j
			for j < len(s) && alnum(s[j]) {
				j++
			}
			if j == st {
				break
			}
			segs++
			if segs < 3 {
				if j >= len(s) || s[j] != '/' {
					break
				}
				j++
			}
		}
		if segs == 3 {
			out = append(out, [2]int{i, j})
		}
	}
}

func sendgridScan(s string) [][2]int {
	var out [][2]int
	for off := 0; ; {
		i := strings.Index(s[off:], "SG.")
		if i < 0 {
			return out
		}
		i += off
		off = i + 1
		if !boundaryBefore(s, i) {
			continue
		}
		j := i + 3
		a := j
		for j < len(s) && base64URLChar(s[j]) {
			j++
		}
		if j-a != 22 || j >= len(s) || s[j] != '.' {
			continue
		}
		j++
		b := j
		for j < len(s) && base64URLChar(s[j]) {
			j++
		}
		if j-b == 43 {
			out = append(out, [2]int{i, j})
		}
	}
}

// jwtScan matches header.payload.signature where header and payload start
// with "eyJ" (base64url of `{"`) and the header decodes to JSON naming an
// "alg".
func jwtScan(s string) [][2]int {
	var out [][2]int
	for off := 0; ; {
		i := strings.Index(s[off:], "eyJ")
		if i < 0 {
			return out
		}
		i += off
		off = i + 1
		if !boundaryBefore(s, i) {
			continue
		}
		j := i
		var parts [3][2]int
		k := 0
		for k < 3 {
			st := j
			for j < len(s) && base64URLChar(s[j]) {
				j++
			}
			parts[k] = [2]int{st, j}
			if j-st < 10 {
				break
			}
			k++
			if k < 3 {
				if j >= len(s) || s[j] != '.' {
					break
				}
				j++
			}
		}
		if k != 3 || !strings.HasPrefix(s[parts[1][0]:], "eyJ") {
			continue
		}
		hdr, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s[parts[0][0]:parts[0][1]], "="))
		if err != nil || !bytes.Contains(hdr, []byte(`"alg"`)) {
			continue
		}
		out = append(out, [2]int{i, j})
		off = j
	}
}

// privateKeyScan matches a PEM private key block, header to footer (or 16
// KiB when the footer is missing).
func privateKeyScan(s string) [][2]int {
	var out [][2]int
	for off := 0; ; {
		i := strings.Index(s[off:], "-----BEGIN ")
		if i < 0 {
			return out
		}
		i += off
		off = i + 1
		hdrEnd := strings.Index(s[i+11:], "-----")
		if hdrEnd < 0 || hdrEnd > 40 {
			continue
		}
		label := s[i+11 : i+11+hdrEnd]
		if !strings.HasSuffix(label, "PRIVATE KEY") && !strings.HasSuffix(label, "PRIVATE KEY BLOCK") {
			continue
		}
		end := strings.Index(s[i:], "-----END "+label+"-----")
		j := len(s)
		if end >= 0 {
			j = i + end + len("-----END "+label+"-----")
		}
		j = min(j, i+16<<10)
		out = append(out, [2]int{i, j})
		off = j
	}
}

var connSchemes = []string{"postgres", "postgresql", "mysql", "mariadb", "mongodb", "mongodb+srv", "redis", "rediss",
	"amqp", "amqps", "mssql", "sqlserver", "jdbc:postgresql", "jdbc:mysql"}

// connStringScan matches scheme://user:password@ and reports the password
// only (the part that is secret).
func connStringScan(schemes []string) func(string) [][2]int {
	return func(s string) [][2]int {
		var out [][2]int
		ls := strings.ToLower(s)
		for _, sc := range schemes {
			p := sc + "://"
			for off := 0; ; {
				i := strings.Index(ls[off:], p)
				if i < 0 {
					break
				}
				i += off
				off = i + 1
				if i > 0 && (alnum(s[i-1]) || s[i-1] == '+' || s[i-1] == ':') {
					continue
				}
				j := i + len(p)
				u := j
				for j < len(s) && userChar(s[j]) {
					j++
				}
				if j >= len(s) || s[j] != ':' || j-u > 128 {
					continue
				}
				j++
				pw := j
				for j < len(s) && passChar(s[j]) && j-pw <= 256 {
					j++
				}
				if j >= len(s) || s[j] != '@' || j == pw || placeholderPassword(s[pw:j]) {
					continue
				}
				out = append(out, [2]int{pw, j})
			}
		}
		return out
	}
}

func userChar(c byte) bool {
	return c > 0x20 && c < 0x7f && c != ':' && c != '/' && c != '@' && c != '"' && c != '\'' && c != '`' && c != '<' && c != '>'
}
func passChar(c byte) bool {
	return c > 0x20 && c < 0x7f && c != '@' && c != '/' && c != '"' && c != '\'' && c != '`' && c != '<' && c != '>'
}

// placeholderPassword rejects documentation placeholders.
func placeholderPassword(p string) bool {
	lp := strings.ToLower(p)
	switch lp {
	case "password", "pass", "passwd", "secret", "pwd", "changeme", "xxx", "xxxx", "****", "***", "user", "example":
		return true
	}
	return strings.HasPrefix(lp, "${") || strings.HasPrefix(lp, "{{") || strings.HasPrefix(lp, "<") ||
		strings.HasPrefix(lp, "%") || strings.HasPrefix(lp, "$") || strings.HasPrefix(lp, "[redacted")
}

// genericKeys are the key names of the low tier.
var genericKeys = []string{"api_key", "apikey", "api-key", "secret", "client_secret", "access_token", "auth_token",
	"password", "passwd", "private_key", "secret_key", "token", "access_key"}

// genericScan is the low-confidence entropy tier: a secret-like key
// assigned a quoted value of 16..128 token characters that mixes letters
// and digits and has Shannon entropy of at least 3.5 bits per character.
func genericScan(s string) [][2]int {
	var out [][2]int
	ls := strings.ToLower(s)
	for _, k := range genericKeys {
		for off := 0; ; {
			i := strings.Index(ls[off:], k)
			if i < 0 {
				break
			}
			i += off
			off = i + 1
			end := i + len(k)
			// Whole identifier only (a "token" inside "csrftoken" is a
			// different key).
			if !boundaryBefore(s, i) && s[i-1] != '_' && s[i-1] != '-' || end < len(s) && alnumUnderscore(s[end]) {
				continue
			}
			st, en, ok := assignedValue(s, end, 16, 128, func(c byte) bool { return alnum(c) || strings.IndexByte("+/=_.~-", c) >= 0 })
			if !ok || st == 0 || !strings.ContainsRune("\"'`", rune(s[st-1])) || en >= len(s) || s[en] != s[st-1] {
				continue
			}
			if v := s[st:en]; highEntropy(v) {
				out = append(out, [2]int{st, en})
			}
		}
	}
	return out
}

func highEntropy(v string) bool {
	letter, digit := false, false
	var counts [256]int
	for i := 0; i < len(v); i++ {
		c := v[i]
		counts[c]++
		letter = letter || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		digit = digit || c >= '0' && c <= '9'
	}
	if !letter || !digit || isPlaceholder(v) {
		return false
	}
	h := 0.0
	for _, n := range counts {
		if n > 0 {
			p := float64(n) / float64(len(v))
			h -= p * math.Log2(p)
		}
	}
	return h >= 3.5
}

// ScanSecrets runs every rule over text and returns the findings sorted
// by (start, end, rule), with overlaps resolved: a high-tier match beats
// a low-tier one, then the earlier and longer match wins. Text inside an
// existing redaction placeholder is skipped (Redact idempotence).
func ScanSecrets(text []byte, doc string) []SecretFinding {
	s := string(text)
	holes := placeholderSpans(s)
	var all []SecretFinding
	for _, r := range secretRules {
		for _, sp := range r.scan(s) {
			if overlapsAny(sp, holes) {
				continue
			}
			sec := s[sp[0]:sp[1]]
			all = append(all, SecretFinding{RuleID: r.id, Tier: r.tier, Start: sp[0], End: sp[1],
				Preview: oam.SecretPreview(sec), Doc: doc, Secret: []byte(sec)})
		}
	}
	slices.SortFunc(all, func(a, b SecretFinding) int {
		return cmp.Or(cmp.Compare(tierRank(a.Tier), tierRank(b.Tier)), cmp.Compare(a.Start, b.Start),
			cmp.Compare(b.End, a.End), cmp.Compare(a.RuleID, b.RuleID))
	})
	var kept []SecretFinding
	for _, f := range all {
		if !slices.ContainsFunc(kept, func(k SecretFinding) bool { return f.Start < k.End && k.Start < f.End }) {
			kept = append(kept, f)
		}
	}
	slices.SortFunc(kept, func(a, b SecretFinding) int {
		return cmp.Or(cmp.Compare(a.Start, b.Start), cmp.Compare(a.End, b.End), cmp.Compare(a.RuleID, b.RuleID))
	})
	return kept
}

func tierRank(t SecretTier) int {
	if t == TierHigh {
		return 0
	}
	return 1
}

func overlapsAny(sp [2]int, holes [][2]int) bool {
	for _, h := range holes {
		if sp[0] < h[1] && h[0] < sp[1] {
			return true
		}
	}
	return false
}

// redactPrefix starts every redaction placeholder.
const redactPrefix = "[REDACTED:"

// placeholderSpans finds existing "[REDACTED:...]" placeholders.
func placeholderSpans(s string) [][2]int {
	var out [][2]int
	for off := 0; ; {
		i := strings.Index(s[off:], redactPrefix)
		if i < 0 {
			return out
		}
		i += off
		j := strings.IndexByte(s[i:], ']')
		if j < 0 || j > 96 {
			off = i + 1
			continue
		}
		out = append(out, [2]int{i, i + j + 1})
		off = i + j + 1
	}
}

// secretShape summarizes a secret's character classes: "A" upper case,
// "a" lower case, "9" digit, "_" anything else, in that order.
func secretShape(s []byte) string {
	var up, lo, dg, ot bool
	for _, c := range s {
		switch {
		case c >= 'A' && c <= 'Z':
			up = true
		case c >= 'a' && c <= 'z':
			lo = true
		case c >= '0' && c <= '9':
			dg = true
		default:
			ot = true
		}
	}
	var b strings.Builder
	for _, x := range []struct {
		on bool
		c  byte
	}{{up, 'A'}, {lo, 'a'}, {dg, '9'}, {ot, '_'}} {
		if x.on {
			b.WriteByte(x.c)
		}
	}
	return b.String()
}

// RedactionPlaceholder is the typed placeholder that replaces a secret:
// "[REDACTED:<rule_id>:<shape>:<length>]".
func RedactionPlaceholder(rule string, secret []byte) string {
	return redactPrefix + rule + ":" + secretShape(secret) + ":" + strconv.Itoa(len(secret)) + "]"
}

// Redact replaces every credential-shaped string (every secret rule,
// both tiers, connection-string passwords included) by its typed
// placeholder (V2 §12.11). It is applied to anything that will be hashed
// for the index, indexed, embedded or logged: normalized snapshots and
// diff hunks. Redact is idempotent (CONTENT-INV-10) and returns the
// findings it replaced.
func Redact(text []byte) ([]byte, []SecretFinding) {
	fs := ScanSecrets(text, "")
	if len(fs) == 0 {
		return text, nil
	}
	var b bytes.Buffer
	b.Grow(len(text))
	prev := 0
	for _, f := range fs {
		b.Write(text[prev:f.Start])
		b.WriteString(RedactionPlaceholder(f.RuleID, f.Secret))
		prev = f.End
	}
	b.Write(text[prev:])
	return b.Bytes(), fs
}
