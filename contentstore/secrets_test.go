package contentstore

import (
	"encoding/base64"
	"strings"

	"github.com/genuinebnt/flowforge/core"
)

const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func token(r core.Rng, n int, alphabet string) string {
	var b strings.Builder
	for range n {
		b.WriteByte(alphabet[core.IntN(r, len(alphabet))])
	}
	return b.String()
}

// genSecret returns a random secret of one named rule: the text to plant
// and the secret bytes that must never be stored in the clear. Provider
// prefixes are joined from pieces at run time, so no format-valid key is
// written in this file (push protection).
func genSecret(r core.Rng, rule string) (plant, secret string) {
	upper := "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b64 := alnum + "+/"
	j := func(parts ...string) string { return strings.Join(parts, "") }
	switch rule {
	case "aws_access_key_id":
		s := j("AK", "IA", token(r, 16, upper))
		return s, s
	case "aws_secret_access_key":
		s := token(r, 40, b64)
		return `aws_secret_access_key = "` + s + `"`, s
	case "github_token":
		s := j("gh", "p_", token(r, 36, alnum))
		return s, s
	case "github_fine_grained_pat":
		s := j("github", "_pat_", token(r, 82, alnum+"_"))
		return s, s
	case "gitlab_token":
		s := j("gl", "pat-", token(r, 20, alnum+"_-"))
		return s, s
	case "slack_token":
		s := j("xo", "xb-", token(r, 40, alnum+"-"))
		return s, s
	case "slack_webhook":
		s := j("https://hooks.", "slack.com/services/T", token(r, 8, upper), "/B", token(r, 8, upper), "/", token(r, 24, alnum))
		return s, s
	case "stripe_secret_key":
		s := j("sk", "_live_", token(r, 32, alnum))
		return s, s
	case "google_api_key":
		s := j("AI", "za", token(r, 35, alnum+"_-"))
		return s, s
	case "openai_api_key":
		s := j("sk", "-proj-", token(r, 48, alnum+"_-"))
		return s, s
	case "anthropic_api_key":
		s := j("sk", "-ant-", token(r, 64, alnum+"_-"))
		return s, s
	case "sendgrid_api_key":
		s := j("S", "G.", token(r, 22, alnum+"_-"), ".", token(r, 43, alnum+"_-"))
		return s, s
	case "npm_token":
		s := j("np", "m_", token(r, 36, alnum))
		return s, s
	case "azure_storage_key":
		s := token(r, 86, b64) + "=="
		return "DefaultEndpointsProtocol=https;AccountName=x;AccountKey=" + s + ";", s
	case "jwt":
		h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
		p := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"` + token(r, 12, alnum) + `"}`))
		s := h + "." + p + "." + token(r, 43, alnum+"_-")
		return s, s
	case "private_key":
		body := token(r, 64, b64) + "\n" + token(r, 64, b64)
		return j("-----BEGIN RSA ", "PRIVATE KEY-----\n", body, "\n-----END RSA ", "PRIVATE KEY-----"), body[:64]
	case "connection_string":
		pw := token(r, 18, alnum)
		return "postgres://app_user:" + pw + "@db.internal.example:5432/prod", pw
	case "url_credentials":
		pw := token(r, 14, alnum)
		return "https://deploy:" + pw + "@ci.internal.example/hook", pw
	}
	panic("no generator for " + rule)
}

// secretRules are the named high-tier rules the generator covers.
var secretRules = []string{"aws_access_key_id", "aws_secret_access_key", "github_token", "github_fine_grained_pat", "gitlab_token",
	"slack_token", "slack_webhook", "stripe_secret_key", "google_api_key", "openai_api_key", "anthropic_api_key",
	"sendgrid_api_key", "npm_token", "azure_storage_key", "jwt", "private_key", "connection_string", "url_credentials"}
