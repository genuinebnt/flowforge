package content

import (
	"bytes"
	"slices"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/sketch"
)

// IntegrityFlag names one collection-integrity problem (V2 §12.5).
type IntegrityFlag string

// Integrity flags. fetch_failed makes the capture failed, truncated makes
// it partial, and every other flag makes it degraded.
const (
	FlagFetchFailed       IntegrityFlag = "fetch_failed"
	FlagTruncated         IntegrityFlag = "truncated"
	FlagLoginRedirect     IntegrityFlag = "login_redirect"
	FlagChallenge         IntegrityFlag = "challenge_page"
	FlagErrorAfterSuccess IntegrityFlag = "error_after_success"
	FlagSoftError         IntegrityFlag = "soft_error_after_success"
	FlagMassRemoval       IntegrityFlag = "mass_removal"
	FlagCatchAll          IntegrityFlag = "catch_all"
)

// PreviousCapture summarizes the last complete capture of the same (URL,
// profile), as the store keeps it.
type PreviousCapture struct {
	Status int `json:"status"`
	// FeatureIDs are the sorted feature ids of that capture.
	FeatureIDs []string `json:"feature_ids,omitempty"`
	// ErrorPage is LooksLikeErrorPage of that capture's body.
	ErrorPage bool `json:"error_page,omitempty"`
}

// HostBody is the latest body sketch of another URL of the same host.
type HostBody struct {
	URL     string `json:"url"`
	SimHash uint64 `json:"simhash"`
}

// IntegrityInput is what CheckIntegrity looks at.
type IntegrityInput struct {
	Capture *Capture
	// Body is the (possibly cut) body that was read.
	Body []byte
	// FeatureIDs are the current capture's sorted feature ids.
	FeatureIDs []string
	// SimHash is BodySimHash of the current normalized body.
	SimHash  uint64
	Previous *PreviousCapture
	// HostBodies are the other URLs of the host, for catch-all detection.
	HostBodies []HostBody
	// CatchAllDistance is the Hamming distance under which two bodies are
	// "the same page" (0: 3, the exact range of sketch's multi-index).
	CatchAllDistance int
}

// IntegrityReport is the verdict.
type IntegrityReport struct {
	Outcome Outcome         `json:"outcome"`
	Flags   []IntegrityFlag `json:"flags,omitempty"`
	// Reasons explain each flag (stable "flag:detail" strings).
	Reasons []string `json:"reasons,omitempty"`
}

// Degraded reports an outcome that must not diff or withdraw.
func (r IntegrityReport) Degraded() bool {
	return r.Outcome == OutcomeDegraded || r.Outcome == OutcomeFailed
}

// BodySimHash is the SimHash of a normalized body's 3-token shingles
// (sketch.Tokens), the catch-all and soft-404 sketch.
func BodySimHash(normalized []byte) uint64 {
	return sketch.SimHash(sketch.Shingles(sketch.Tokens(string(normalized)), 3))
}

// loginMarkers are path and query fragments of login and SSO endpoints.
var loginMarkers = []string{"/login", "/signin", "/sign-in", "/sign_in", "/logon", "/auth/", "/sso", "/oauth/authorize",
	"/oauth2/authorize", "/saml", "/session/new", "/users/sign_in", "/account/login", "/accounts/login",
	"/wp-login.php", "/cas/login", "/idp/", "returnurl=", "return_to=", "redirect_uri=", "/authorize?"}

func hasLoginMarker(u string) (string, bool) {
	lu := strings.ToLower(u)
	for _, m := range loginMarkers {
		if strings.Contains(lu, m) {
			return m, true
		}
	}
	return "", false
}

// challengeMarker is one bot-challenge or CAPTCHA signature. Strong
// markers are interstitial pages by themselves; weak ones (an embedded
// CAPTCHA widget, which a real login form may carry) count only with a
// blocking status (403, 429, 503).
type challengeMarker struct {
	name, text string
	strong     bool
}

var challengeMarkers = []challengeMarker{
	{"cloudflare", "/cdn-cgi/challenge-platform/", true}, {"cloudflare", "cf-chl-", true},
	{"cloudflare", "cf_chl_opt", true}, {"cloudflare", "<title>just a moment...</title>", true},
	{"cloudflare", "attention required! | cloudflare", true}, {"imperva", "_incapsula_resource", true},
	{"imperva", "incapsula incident id", true}, {"perimeterx", "px-captcha", true},
	{"datadome", "captcha-delivery.com", true}, {"aws_waf", "awswafintegration", true},
	{"sucuri", "sucuri website firewall", true}, {"generic", "checking your browser before accessing", true},
	{"generic", "verify you are human", true}, {"generic", "are you a robot", true},
	{"recaptcha", "g-recaptcha", false}, {"recaptcha", "www.google.com/recaptcha", false},
	{"hcaptcha", "h-captcha", false}, {"turnstile", "challenges.cloudflare.com/turnstile", false},
	{"akamai", "access denied", false}, {"generic", "captcha", false},
}

// htmlTitle returns the lower-cased text of the first <title> element
// found by a byte scan (good enough for a verdict; no full parse).
func htmlTitle(lb []byte) string {
	i := bytes.Index(lb, []byte("<title"))
	if i < 0 {
		return ""
	}
	j := bytes.IndexByte(lb[i:], '>')
	if j < 0 {
		return ""
	}
	k := bytes.Index(lb[i+j:], []byte("</title"))
	if k < 0 || k > 512 {
		return ""
	}
	return collapseSpace(string(lb[i+j+1 : i+j+k]))
}

var errorTitleWords = []string{"404", "not found", "page not found", "500", "internal server error", "502",
	"bad gateway", "503", "service unavailable", "maintenance", "error"}

// LooksLikeErrorPage reports a 2xx body that is an error page in
// disguise (a soft 404): its <title> names an HTTP error or maintenance.
//
// PORT: bytes.ToLower here and in CheckIntegrity is Unicode-aware (simple
// mappings, so the Kelvin sign lowers to "k" and U+0130 to "i") and turns
// each invalid UTF-8 byte into U+FFFD; the ASCII markers then match in
// text an ASCII-only lowering would miss. Lower the same way.
func LooksLikeErrorPage(body []byte) bool {
	lb := bytes.ToLower(body[:min(len(body), 64<<10)])
	t := htmlTitle(lb)
	for _, w := range errorTitleWords {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}

// CheckIntegrity applies the collection-integrity checks of V2 §12.5:
// fetch failure, truncation, a login redirect, a challenge or CAPTCHA
// page, an error (hard or soft) where the previous capture succeeded, a
// mass removal (more than half of the previous features gone), and a
// catch-all body (the same body served for at least two other URLs of the
// host, so three or more URLs in all). The outcome only gets worse than
// the collector's own.
func CheckIntegrity(in IntegrityInput) IntegrityReport {
	c := in.Capture
	out := c.Outcome
	if out == "" {
		out = OutcomeComplete
	}
	var r IntegrityReport
	flag := func(f IntegrityFlag, detail string, o Outcome) {
		r.Flags = append(r.Flags, f)
		r.Reasons = append(r.Reasons, string(f)+":"+detail)
		out = worseOutcome(out, o)
	}
	if c.Status == 0 || out == OutcomeFailed {
		flag(FlagFetchFailed, "no response", OutcomeFailed)
		r.Outcome = OutcomeFailed
		return r
	}
	if c.Truncated || len(c.Body) > MaxResponseBytes {
		flag(FlagTruncated, "body cut at the size limit", OutcomePartial)
	}
	if _, reqLogin := hasLoginMarker(c.URL); !reqLogin {
		for _, h := range c.RedirectChain {
			if loc, ok := headerValue(h.Headers, "location"); ok {
				if m, ok := hasLoginMarker(loc); ok {
					flag(FlagLoginRedirect, m, OutcomeDegraded)
					break
				}
			}
		}
	}
	lb := bytes.ToLower(in.Body[:min(len(in.Body), 64<<10)])
	blocking := c.Status == 403 || c.Status == 429 || c.Status == 503
	for _, m := range challengeMarkers {
		if (m.strong || blocking) && bytes.Contains(lb, []byte(m.text)) {
			flag(FlagChallenge, m.name, OutcomeDegraded)
			break
		}
	}
	if p := in.Previous; p != nil && p.Status >= 200 && p.Status < 300 {
		switch {
		case c.Status >= 400:
			flag(FlagErrorAfterSuccess, "status "+itoa(c.Status), OutcomeDegraded)
		case c.Status < 300 && !p.ErrorPage && LooksLikeErrorPage(in.Body):
			flag(FlagSoftError, "error title", OutcomeDegraded)
		}
	}
	if p := in.Previous; p != nil && len(p.FeatureIDs) >= 4 {
		gone := 0
		for _, id := range p.FeatureIDs {
			if _, ok := slices.BinarySearch(in.FeatureIDs, id); !ok {
				gone++
			}
		}
		if 2*gone > len(p.FeatureIDs) {
			flag(FlagMassRemoval, itoa(gone)+"/"+itoa(len(p.FeatureIDs)), OutcomeDegraded)
		}
	}
	if len(in.HostBodies) > 0 && len(bytes.TrimSpace(in.Body)) > 0 {
		d := in.CatchAllDistance
		if d <= 0 {
			d = 3
		}
		seen := map[string]bool{}
		for _, h := range in.HostBodies {
			if h.URL != c.URL && !seen[h.URL] && sketch.Hamming(h.SimHash, in.SimHash) <= d {
				seen[h.URL] = true
			}
		}
		if len(seen) >= 2 {
			flag(FlagCatchAll, itoa(len(seen)+1)+" urls", OutcomeDegraded)
		}
	}
	r.Outcome = out
	return r
}

func itoa(n int) string { return strconv.Itoa(n) }
