package content

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

const b64Chars = alnumChars + "+/"

// genSecret returns a random secret of one named rule: the full text to
// plant and the secret part that must never appear in indexable output.
func genSecret(r core.Rng, rule string) (plant, secret string) {
	tok := func(n int, a string) string { return randToken(r, n, a) }
	upper := "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	switch rule {
	case "aws_access_key_id":
		s := "AKIA" + tok(16, upper)
		return s, s
	case "aws_secret_access_key":
		s := tok(40, b64Chars)
		return `aws_secret_access_key = "` + s + `"`, s
	case "github_token":
		s := "ghp_" + tok(36, alnumChars)
		return s, s
	case "github_fine_grained_pat":
		s := "github_pat_" + tok(82, alnumChars+"_")
		return s, s
	case "gitlab_token":
		s := "glpat-" + tok(20, alnumChars+"_-")
		return s, s
	case "slack_token":
		s := "xoxb-" + tok(40, alnumChars+"-")
		return s, s
	case "slack_webhook":
		s := "https://hooks.slack.com/services/T" + tok(8, upper) + "/B" + tok(8, upper) + "/" + tok(24, alnumChars)
		return s, s
	case "stripe_secret_key":
		s := "sk_live_" + tok(32, alnumChars)
		return s, s
	case "google_api_key":
		s := "AIza" + tok(35, alnumChars+"_-")
		return s, s
	case "openai_api_key":
		s := "sk-proj-" + tok(48, alnumChars+"_-")
		return s, s
	case "anthropic_api_key":
		s := "sk-ant-" + tok(64, alnumChars+"_-")
		return s, s
	case "sendgrid_api_key":
		s := "SG." + tok(22, alnumChars+"_-") + "." + tok(43, alnumChars+"_-")
		return s, s
	case "npm_token":
		s := "npm_" + tok(36, alnumChars)
		return s, s
	case "azure_storage_key":
		s := tok(86, b64Chars) + "=="
		return "DefaultEndpointsProtocol=https;AccountName=x;AccountKey=" + s + ";", s
	case "jwt":
		h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
		p := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"` + tok(12, alnumChars) + `"}`))
		s := h + "." + p + "." + tok(43, alnumChars+"_-")
		return s, s
	case "private_key":
		s := "-----BEGIN RSA PRIVATE KEY-----\n" + tok(64, b64Chars) + "\n" + tok(64, b64Chars) + "\n-----END RSA PRIVATE KEY-----"
		return s, tok(0, "") + s[32:96]
	case "connection_string":
		pw := tok(18, alnumChars+"-_.~!")
		return "postgres://app_user:" + pw + "@db.internal.example:5432/prod", pw
	case "url_credentials":
		pw := tok(14, alnumChars)
		return "https://deploy:" + pw + "@ci.internal.example/hook", pw
	case "generic_high_entropy":
		s := tok(28, "abcdefghijklmnop0123456789QRSTUV")
		return `api_key: "` + s + `"`, s
	}
	panic("no generator for " + rule)
}

// plantIn places a secret in a document of each kind, in a typical spot.
func plantIn(kind Kind, plant string) (string, []byte) {
	js := strings.NewReplacer(`"`, `\"`, "\n", `\n`).Replace(plant)
	switch kind {
	case KindHTML:
		return "https://r.example/", []byte(`<html><body><p>config: ` + strings.ReplaceAll(plant, "<", "&lt;") + `</p><script>var cfg = "` + js + `";</script></body></html>`)
	case KindJS:
		return "https://r.example/app.js", []byte(`var cfg = {k: "` + js + `"}; fetch("/api/x");`)
	case KindJSON:
		b, _ := json.Marshal(map[string]string{"config": plant})
		return "https://r.example/config.json", b
	}
	return "https://r.example/notes.txt", []byte("notes\n" + plant + "\nend\n")
}

// CONTENT-INV-10: redaction removes every planted secret (every named
// rule and the entropy tier, connection-string passwords included) from
// every indexable output: normalized snapshots, diff hunks and
// side-by-side rows, features, change sets and observations. Redact is
// idempotent.
func TestContentInv10_RedactionRemovesSecrets(t *testing.T) {
	r := testkit.Rng(t)
	rules := SecretRuleIDs()
	empty := map[Kind]*Processed{}
	for range max(60, testkit.Cases()/10) {
		rule := pick(r, rules)[0]
		plant, secret := genSecret(r, rule)
		kind := pick(r, []Kind{KindHTML, KindJS, KindJSON, KindText})
		u, body := plantIn(kind, plant)
		p := mustProcess(t, capture(u, body, nil, t0), ProcessOptions{})
		if empty[kind] == nil {
			_, eb := plantIn(kind, "nothing")
			empty[kind] = mustProcess(t, capture(u, eb, nil, t0), ProcessOptions{})
		}
		prev := relabel(empty[kind], u)
		cs, err := Compare(prev, p, DiffOptions{})
		if err != nil {
			t.Fatal(err)
		}
		o, err := Observe(p, ObserveOptions{Hasher: testHasher})
		if err != nil {
			t.Fatalf("%s: %v", rule, err)
		}
		ob, _ := ObservationJSON(o)
		fb, _ := json.Marshal(p.Features())
		cb, _ := json.Marshal(cs)
		outputs := map[string][]byte{"features": fb, "changeset": cb, "observation": ob}
		for _, a := range p.Artifacts {
			outputs["normalized "+a.DocID] = a.Normalized.Text
		}
		for name, out := range outputs {
			if bytes.Contains(out, []byte(secret)) {
				t.Fatalf("rule %s in %s: secret survives in %s\n%s", rule, kind, name, out)
			}
		}
		if rule != "generic_high_entropy" && !hasFinding(p, rule) {
			t.Fatalf("rule %s in %s: no finding", rule, kind)
		}
		once, _ := Redact(body)
		twice, again := Redact(once)
		if !bytes.Equal(once, twice) || len(again) != 0 {
			t.Fatalf("rule %s: Redact not idempotent\n%s\n%s", rule, once, twice)
		}
	}
}

func hasFinding(p *Processed, rule string) bool {
	for _, f := range p.Findings() {
		if f.RuleID == rule {
			return true
		}
	}
	return false
}

// FuzzContentRedactIdempotent: Redact(Redact(x)) == Redact(x) and the second
// pass finds nothing (CONTENT-INV-10), and the reported findings locate
// their secrets in the original text. The seed is the case
// FuzzCStoreSealRoundTrip found: redacting the AWS key id exposed the
// connection-string password glued to it.
func FuzzContentRedactIdempotent(f *testing.F) {
	f.Add([]byte("ABIA0000000000000000postgres://:0@"))
	f.Add([]byte("x AK" + "IAIOSFODNN7EXAMPLEmysql://root:hunter2hunter2@db/x"))
	f.Add([]byte("[REDACTED:jwt:Aa9_:3] plain"))
	f.Fuzz(func(t *testing.T, text []byte) {
		once, fs := Redact(text)
		twice, again := Redact(once)
		if !bytes.Equal(once, twice) || len(again) != 0 {
			t.Fatalf("not idempotent:\n once %q\ntwice %q\nagain %v", once, twice, again)
		}
		for i, f := range fs {
			if f.Start < 0 || f.End > len(text) || f.Start >= f.End || !bytes.Equal(text[f.Start:f.End], f.Secret) {
				t.Fatalf("finding %d [%d,%d) does not locate %q in the input", i, f.Start, f.End, f.Secret)
			}
			if i > 0 && fs[i-1].End > f.Start {
				t.Fatalf("findings %d and %d overlap", i-1, i)
			}
		}
	})
}
