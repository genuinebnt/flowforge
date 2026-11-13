package wellknown

import (
	"bytes"
	"strconv"
	"strings"
)

// RobotsMaxBytes is the RFC 9309 §2.5 parsing limit: crawlers must parse
// at least 500 KiB; content past it is ignored.
const RobotsMaxBytes = 500 << 10

// RobotsOptions configure ParseRobots. The zero value uses the defaults.
type RobotsOptions struct {
	// MaxBytes bounds the bytes parsed (0 = RobotsMaxBytes).
	MaxBytes int
	// MaxItems bounds rules plus sitemap lines kept (0 = DefaultMaxItems).
	MaxItems int
}

// Rule is one allow or disallow line. Pattern is percent-normalized (see
// normalizeOctets); "*" matches any sequence and a final "$" anchors the
// end (RFC 9309 §2.2.3).
type Rule struct {
	Allow   bool
	Pattern string
	Line    int
}

// Group is one group (RFC 9309 §2.1): the product tokens of its
// user-agent lines (lower case; "*" for the catch-all) and its rules.
type Group struct {
	Agents []string
	Rules  []Rule
	// CrawlDelayMillis is the non-standard crawl-delay extension in
	// milliseconds, or -1 when absent.
	CrawlDelayMillis int64
	Line             int
}

// RobotsCode is a stable diagnostic code.
type RobotsCode uint8

const (
	RobotsNone             RobotsCode = iota
	RobotsInvalidLine                 // no ":" separator: ignored
	RobotsUnknownKey                  // a key other than user-agent, allow, disallow, sitemap, crawl-delay: ignored
	RobotsRuleOutsideGroup            // allow/disallow before any user-agent: ignored (§2.2.2)
	RobotsInvalidUserAgent            // user-agent value without a product token: ignored
	RobotsInvalidPattern              // a pattern not starting with "/" or "*": kept, it matches no path
	RobotsInvalidCrawlDelay           // crawl-delay that is not a non-negative number: ignored
	RobotsInputTooLarge               // content past MaxBytes was ignored
	RobotsLimitExceeded               // rules or sitemaps past MaxItems were ignored
)

var robotsCodeNames = [...]string{
	"none", "invalid_line", "unknown_key", "rule_outside_group",
	"invalid_user_agent", "invalid_pattern", "invalid_crawl_delay",
	"input_too_large", "limit_exceeded",
}

// String returns the stable name of c.
func (c RobotsCode) String() string {
	if int(c) < len(robotsCodeNames) {
		return robotsCodeNames[c]
	}
	return "code(" + strconv.Itoa(int(c)) + ")"
}

// RobotsDiag is one diagnostic with its 1-based line number.
type RobotsDiag struct {
	Code RobotsCode
	Line int
}

// Robots is a parsed robots.txt.
type Robots struct {
	Groups []Group
	// Sitemaps lists sitemap URLs in file order (§2.3.5: independent of
	// groups).
	Sitemaps []string
	Diags    []RobotsDiag
	Grade    Grade
}

// robotsState is the line-oriented parser state (RFC 9309 §2.2 grammar:
// group = startgroupline *(startgroupline / emptyline) *(rule / emptyline)).
type robotsState uint8

const (
	// robotsStateStart: before the first user-agent line; rules here
	// belong to no group.
	robotsStateStart robotsState = iota
	// robotsStateAgents: inside a run of startgroupline (user-agent)
	// lines; another user-agent line joins the same group.
	robotsStateAgents
	// robotsStateRules: after the group's first rule; a user-agent line
	// starts a new group.
	robotsStateRules
)

// ParseRobots parses a robots.txt body. It never fails; malformed lines
// are reported and skipped, as RFC 9309 §2.2 asks crawlers to do.
func ParseRobots(data []byte, opts RobotsOptions) *Robots {
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = RobotsMaxBytes
	}
	maxItems := opts.MaxItems
	if maxItems <= 0 {
		maxItems = DefaultMaxItems
	}
	r := &Robots{}
	if len(data) > maxBytes {
		// Cut at the last line break inside the limit: a line split by the
		// limit is not parsed.
		cut := data[:maxBytes]
		if i := bytes.LastIndexAny(cut, "\r\n"); i >= 0 {
			cut = cut[:i+1]
		} else {
			cut = nil
		}
		data = cut
		r.Diags = append(r.Diags, RobotsDiag{Code: RobotsInputTooLarge, Line: 0})
		r.Grade = GradePartial
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	st := robotsStateStart
	items := 0
	diag := func(c RobotsCode, n int) { r.Diags = append(r.Diags, RobotsDiag{Code: c, Line: n}) }
	splitLines(data, func(line []byte, n, _ int) bool {
		if i := bytes.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = trimWS(line)
		if len(line) == 0 {
			return true // emptyline: no effect on grouping
		}
		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			diag(RobotsInvalidLine, n)
			return true
		}
		key := lowerASCII(trimWS(line[:colon]))
		value := trimWS(line[colon+1:])
		switch key {
		case "user-agent":
			tok := productToken(value)
			if tok == "" {
				diag(RobotsInvalidUserAgent, n)
				return true
			}
			if st != robotsStateAgents {
				r.Groups = append(r.Groups, Group{CrawlDelayMillis: -1, Line: n})
			}
			g := &r.Groups[len(r.Groups)-1]
			g.Agents = append(g.Agents, tok)
			st = robotsStateAgents
		case "allow", "disallow":
			if st == robotsStateStart {
				diag(RobotsRuleOutsideGroup, n)
				return true
			}
			// Even an empty rule ends the group's user-agent run.
			st = robotsStateRules
			if len(value) == 0 {
				return true // "Disallow:" with no path disallows nothing
			}
			if items >= maxItems {
				diag(RobotsLimitExceeded, n)
				r.Grade = GradePartial
				return false
			}
			items++
			if value[0] != '/' && value[0] != '*' {
				diag(RobotsInvalidPattern, n)
			}
			g := &r.Groups[len(r.Groups)-1]
			g.Rules = append(g.Rules, Rule{Allow: key == "allow", Pattern: normalizeOctets(string(value)), Line: n})
		case "sitemap":
			if len(value) == 0 {
				return true
			}
			if items >= maxItems {
				diag(RobotsLimitExceeded, n)
				r.Grade = GradePartial
				return false
			}
			items++
			r.Sitemaps = append(r.Sitemaps, string(value))
		case "crawl-delay":
			ms, ok := parseDelay(string(value))
			if !ok || st == robotsStateStart {
				diag(RobotsInvalidCrawlDelay, n)
				return true
			}
			r.Groups[len(r.Groups)-1].CrawlDelayMillis = ms
		default:
			diag(RobotsUnknownKey, n)
		}
		return true
	})
	return r
}

// productToken returns the lower-cased product token of a user-agent
// value: "*", or its leading run of letters, "_" and "-" (RFC 9309
// §2.2.1), so "Googlebot/2.1" names "googlebot".
func productToken(v []byte) string {
	if len(v) > 0 && v[0] == '*' {
		return "*"
	}
	i := 0
	for i < len(v) && isTokenChar(v[i]) {
		i++
	}
	return lowerASCII(v[:i])
}

func isTokenChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == '-'
}

// parseDelay reads a non-negative decimal number of seconds into
// milliseconds (fractions below a millisecond are truncated).
func parseDelay(v string) (int64, bool) {
	if v == "" {
		return 0, false
	}
	intPart, frac, _ := strings.Cut(v, ".")
	if intPart == "" && frac == "" {
		return 0, false
	}
	var ms int64
	for i := 0; i < len(intPart); i++ {
		c := intPart[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		ms = ms*10 + int64(c-'0')
		if ms > 1<<40 {
			return 0, false
		}
	}
	ms *= 1000
	scale := int64(100)
	for i := 0; i < len(frac); i++ {
		c := frac[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		ms += int64(c-'0') * scale
		scale /= 10
	}
	return ms, true
}

func isUnreserved(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
		c == '-' || c == '.' || c == '_' || c == '~'
}

func unhex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

const upperHex = "0123456789ABCDEF"

// normalizeOctets puts a path or pattern in the form RFC 9309 §2.2.2
// compares: percent-encoded unreserved characters are decoded, other
// percent-encodings get upper-case hex, and octets outside printable
// US-ASCII (non-ASCII UTF-8, controls, space) are percent-encoded. "*"
// and "$" are left alone, so patterns keep their operators.
func normalizeOctets(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '%' && i+2 < len(s) && unhex(s[i+1]) >= 0 && unhex(s[i+2]) >= 0:
			v := byte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			if isUnreserved(v) {
				b.WriteByte(v)
			} else {
				b.WriteByte('%')
				b.WriteByte(upperHex[v>>4])
				b.WriteByte(upperHex[v&15])
			}
			i += 2
		case c <= 0x20 || c >= 0x7f:
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&15])
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// matchPattern reports whether pattern matches path (both normalized):
// the pattern matches a prefix of the path, "*" matches any octet
// sequence, and a final "$" requires the match to end at the path's end
// (RFC 9309 §2.2.3). The greedy algorithm backtracks only to the last
// "*", so it runs in O(len(pattern) * len(path)) at worst.
func matchPattern(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = pattern[:len(pattern)-1]
	}
	p, s := 0, 0
	star, mark := -1, 0
	for {
		if p == len(pattern) {
			if !anchored || s == len(path) {
				return true
			}
		} else if pattern[p] == '*' {
			star, mark = p, s
			p++
			continue
		} else if s < len(path) && pattern[p] == path[s] {
			p++
			s++
			continue
		}
		// Mismatch (or anchored pattern ended early): let the last "*"
		// absorb one more octet.
		if star < 0 || mark >= len(path) {
			return false
		}
		mark++
		p, s = star+1, mark
	}
}

// selectGroups returns the groups that apply to a crawler (RFC 9309
// §2.2.1): every group naming its product token (combined), else every
// "*" group (combined).
func (r *Robots) selectGroups(userAgent string) []*Group {
	tok := productToken([]byte(strings.TrimSpace(userAgent)))
	var named, star []*Group
	for i := range r.Groups {
		g := &r.Groups[i]
		for _, a := range g.Agents {
			if a == tok && tok != "*" && tok != "" {
				named = append(named, g)
				break
			}
		}
		for _, a := range g.Agents {
			if a == "*" {
				star = append(star, g)
				break
			}
		}
	}
	if len(named) > 0 {
		return named
	}
	return star
}

// Match returns the deciding rule for userAgent and path (a URL path with
// optional query, "/" if empty) and whether access is allowed: the
// matching rule with the longest pattern wins, allow wins ties, and no
// match allows (RFC 9309 §2.2.2). "/robots.txt" is always allowed
// (§2.2.2, implicitly allowed). rule is nil when no rule matched.
func (r *Robots) Match(userAgent, path string) (rule *Rule, allowed bool) {
	if path == "" {
		path = "/"
	}
	path = normalizeOctets(path)
	if path == "/robots.txt" {
		return nil, true
	}
	var best *Rule
	for _, g := range r.selectGroups(userAgent) {
		for i := range g.Rules {
			ru := &g.Rules[i]
			if best != nil && (len(ru.Pattern) < len(best.Pattern) || (len(ru.Pattern) == len(best.Pattern) && (best.Allow || !ru.Allow))) {
				continue // cannot beat the current best
			}
			if matchPattern(ru.Pattern, path) {
				best = ru
			}
		}
	}
	if best == nil {
		return nil, true
	}
	return best, best.Allow
}

// Allowed reports whether userAgent may fetch path.
func (r *Robots) Allowed(userAgent, path string) bool {
	_, ok := r.Match(userAgent, path)
	return ok
}

// CrawlDelay returns the crawl-delay in milliseconds that applies to
// userAgent: the largest one among its groups, or ok = false.
func (r *Robots) CrawlDelay(userAgent string) (millis int64, ok bool) {
	millis = -1
	for _, g := range r.selectGroups(userAgent) {
		if g.CrawlDelayMillis > millis {
			millis = g.CrawlDelayMillis
		}
	}
	return millis, millis >= 0
}
