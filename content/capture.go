package content

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/oam"
)

// Header is one HTTP header field as received: the name keeps its case
// and order is the order on the wire.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Hop is one redirect response that preceded the final response.
type Hop struct {
	URL     string   `json:"url"`
	Status  int      `json:"status"`
	Headers []Header `json:"headers,omitempty"`
}

// Outcome is how completely a capture was collected (V2 §12.5).
type Outcome string

// Capture outcomes. Only OutcomeComplete can lead to coverage complete.
const (
	// OutcomeComplete: the whole response was read and no integrity
	// check fired.
	OutcomeComplete Outcome = "complete"
	// OutcomePartial: something was read but not all of it (truncated
	// body, a limit hit).
	OutcomePartial Outcome = "partial"
	// OutcomeFailed: nothing usable was read (transport error, status 0).
	OutcomeFailed Outcome = "failed"
	// OutcomeDegraded: a response was read, but it is not the artifact
	// (login redirect, challenge page, error page, catch-all body, mass
	// removal). A degraded capture gets no diff and no withdrawal.
	OutcomeDegraded Outcome = "degraded"
)

// Valid reports whether o is one of the four outcomes.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeComplete, OutcomePartial, OutcomeFailed, OutcomeDegraded:
		return true
	}
	return false
}

// rank orders outcomes from best to worst, so combining two outcomes
// keeps the worse one.
func (o Outcome) rank() int {
	switch o {
	case OutcomeComplete:
		return 0
	case OutcomePartial:
		return 1
	case OutcomeDegraded:
		return 2
	}
	return 3
}

func worseOutcome(a, b Outcome) Outcome {
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// Profile is a capture profile (V2 §5): the request shape a capture was
// made with. Diffs compare only snapshots with the same (URL, profile).
type Profile struct {
	Name   string `json:"name"`
	Method string `json:"method"`
	// Headers are fixed request headers, sorted by (lower-case name,
	// value) by Normalize. Credentials never belong here; a profile that
	// needs a session names a cookie jar instead.
	Headers         []Header `json:"headers,omitempty"`
	UserAgent       string   `json:"user_agent,omitempty"`
	CookieJarRef    string   `json:"cookie_jar_ref,omitempty"`
	FollowRedirects bool     `json:"follow_redirects"`
	Accept          string   `json:"accept,omitempty"`
}

// DefaultProfile is the profile used when a workflow names none: a plain
// GET that follows redirects and sends no cookies.
func DefaultProfile() Profile {
	return Profile{Name: "default", Method: "GET", FollowRedirects: true, Accept: "*/*"}
}

// Normalize validates p and returns it with the method upper-cased,
// header names lower-cased and headers sorted by (name, value).
// Errors wrap ErrInvalidProfile. Authorization, cookie and
// proxy-authorization headers are refused: a profile is stored and shown,
// so it must not carry credentials (spec 18.2).
func (p Profile) Normalize() (Profile, error) {
	if !oam.ValidProfile(p.Name) {
		return p, fmt.Errorf("%w: name %q", ErrInvalidProfile, p.Name)
	}
	m, err := oam.NormalizeHTTPMethod(p.Method)
	if err != nil || m == "*" {
		return p, fmt.Errorf("%w: method %q", ErrInvalidProfile, p.Method)
	}
	p.Method = m
	hs := make([]Header, 0, len(p.Headers))
	for _, h := range p.Headers {
		n := strings.ToLower(strings.TrimSpace(h.Name))
		if n == "" || hasCtl(n) || hasCtl(h.Value) {
			return p, fmt.Errorf("%w: header %q", ErrInvalidProfile, h.Name)
		}
		switch n {
		case "authorization", "cookie", "proxy-authorization":
			return p, fmt.Errorf("%w: credential header %q in a profile", ErrInvalidProfile, n)
		}
		hs = append(hs, Header{Name: n, Value: strings.TrimSpace(h.Value)})
	}
	slices.SortFunc(hs, func(a, b Header) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Value, b.Value))
	})
	if len(hs) == 0 {
		hs = nil
	}
	p.Headers = hs
	return p, nil
}

// Capture is one fetched response, as pure data. The collector fills it;
// this package never performs I/O.
type Capture struct {
	// URL is the requested URL (before redirects).
	URL     string  `json:"url"`
	Profile Profile `json:"profile"`
	// Status is the final HTTP status, 0 when no response arrived.
	Status int `json:"status"`
	// Headers are the final response's headers, raw and in wire order.
	Headers []Header `json:"headers,omitempty"`
	// Body is the final response body as received (after transfer and
	// content decoding).
	Body      []byte    `json:"body,omitempty"`
	FetchedAt time.Time `json:"fetched_at"`
	// RedirectChain lists the redirect responses in order; the final
	// response is not part of it.
	RedirectChain []Hop `json:"redirect_chain,omitempty"`
	// Truncated is set when the collector stopped reading the body at its
	// size limit.
	Truncated bool `json:"truncated,omitempty"`
	// Outcome is the collector's own verdict (complete unless it knows
	// better); CheckIntegrity may only make it worse.
	Outcome Outcome `json:"outcome"`
}

// Validate checks the structural fields; errors wrap ErrInvalidCapture.
func (c *Capture) Validate() error {
	switch {
	case strings.TrimSpace(c.URL) == "":
		return fmt.Errorf("%w: url required", ErrInvalidCapture)
	case c.Status < 0 || c.Status > 999:
		return fmt.Errorf("%w: status %d", ErrInvalidCapture, c.Status)
	case c.Outcome != "" && !c.Outcome.Valid():
		return fmt.Errorf("%w: outcome %q", ErrInvalidCapture, c.Outcome)
	}
	if _, err := c.Profile.Normalize(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidCapture, err)
	}
	return nil
}

// FinalURL is the URL the final response came from: the target of the
// last redirect's Location (resolved against the hop URL), or URL.
func (c *Capture) FinalURL() string {
	cur := c.URL
	for _, h := range c.RedirectChain {
		if loc, ok := headerValue(h.Headers, "location"); ok {
			if r, ok := resolveRef(h.URL, loc); ok {
				cur = r
			}
		}
	}
	return cur
}

// Header returns the first value of the named response header (case
// insensitive).
func (c *Capture) Header(name string) (string, bool) { return headerValue(c.Headers, name) }

// ContentType returns the media type of the Content-Type header in lower
// case, without parameters ("" when absent).
func (c *Capture) ContentType() string {
	v, _ := c.Header("content-type")
	return mediaType(v)
}

func headerValue(hs []Header, name string) (string, bool) {
	for _, h := range hs {
		if strings.EqualFold(h.Name, name) {
			return h.Value, true
		}
	}
	return "", false
}

func headerValues(hs []Header, name string) []string {
	var out []string
	for _, h := range hs {
		if strings.EqualFold(h.Name, name) {
			out = append(out, h.Value)
		}
	}
	return out
}

// mediaType returns "type/subtype" of a Content-Type value, lower-cased.
func mediaType(v string) string {
	mt, _, _ := strings.Cut(v, ";")
	return strings.ToLower(strings.TrimSpace(mt))
}

func hasCtl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 && s[i] != '\t' || s[i] == 0x7f {
			return true
		}
	}
	return false
}
