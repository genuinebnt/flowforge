package wellknown

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/testkit"
)

type robotsFile struct {
	Cases []struct {
		Name      string `json:"name"`
		Robots    string `json:"robots"`
		PrefixPad int    `json:"robots_prefix_pad"`
		Options   struct {
			MaxItems int `json:"max_items"`
		} `json:"options"`
		Groups     *int     `json:"groups"`
		Grade      string   `json:"grade"`
		Diags      []string `json:"diags"`
		Sitemaps   []string `json:"sitemaps"`
		CrawlDelay *struct {
			Agent  string `json:"agent"`
			Millis int64  `json:"millis"`
		} `json:"crawl_delay"`
		Checks []struct {
			Agent   string `json:"agent"`
			Path    string `json:"path"`
			Allowed bool   `json:"allowed"`
		} `json:"checks"`
	} `json:"cases"`
}

// padding returns n bytes of comment lines.
func padding(n int) []byte {
	line := []byte("#" + strings.Repeat("x", 8) + "\n")
	return bytes.Repeat(line, n/len(line)+1)[:n]
}

// TestWKConformanceRobots runs conformance/wellknown/robots.json.
func TestWKConformanceRobots(t *testing.T) {
	var f robotsFile
	testkit.LoadCases[robotsFile](t, "wellknown") // the suite must exist and decode
	if err := unmarshal(testkit.ReadFile(t, "wellknown", "robots.json"), &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			src := append(padding(c.PrefixPad), c.Robots...)
			r := ParseRobots(src, RobotsOptions{MaxItems: c.Options.MaxItems})
			if c.Groups != nil && len(r.Groups) != *c.Groups {
				t.Errorf("%d groups, want %d", len(r.Groups), *c.Groups)
			}
			want := c.Grade
			if want == "" {
				want = "complete"
			}
			if r.Grade.String() != want {
				t.Errorf("grade %s, want %s", r.Grade, want)
			}
			if c.Diags != nil {
				var got []string
				for _, d := range r.Diags {
					got = append(got, d.Code.String())
				}
				if strings.Join(got, ",") != strings.Join(c.Diags, ",") {
					t.Errorf("diags %v, want %v", got, c.Diags)
				}
			}
			if c.Sitemaps != nil && strings.Join(r.Sitemaps, " ") != strings.Join(c.Sitemaps, " ") {
				t.Errorf("sitemaps %v, want %v", r.Sitemaps, c.Sitemaps)
			}
			if c.CrawlDelay != nil {
				if ms, ok := r.CrawlDelay(c.CrawlDelay.Agent); !ok || ms != c.CrawlDelay.Millis {
					t.Errorf("crawl-delay %d %v, want %d", ms, ok, c.CrawlDelay.Millis)
				}
			}
			for _, ch := range c.Checks {
				if got := r.Allowed(ch.Agent, ch.Path); got != ch.Allowed {
					t.Errorf("Allowed(%q, %q) = %v, want %v", ch.Agent, ch.Path, got, ch.Allowed)
				}
			}
		})
	}
	if len(f.Cases) < 20 {
		t.Fatalf("only %d robots cases", len(f.Cases))
	}
}

type securityFile struct {
	Cases []struct {
		Name    string `json:"name"`
		Now     string `json:"now"`
		Text    string `json:"text"`
		Options struct {
			MaxItems int `json:"max_items"`
			MaxBytes int `json:"max_bytes"`
		} `json:"options"`
		Valid              bool            `json:"valid"`
		Signed             bool            `json:"signed"`
		Grade              string          `json:"grade"`
		Diags              []string        `json:"diags"`
		Contact            []string        `json:"contact"`
		Expires            string          `json:"expires"`
		Encryption         []string        `json:"encryption"`
		Policy             []string        `json:"policy"`
		Acknowledgments    []string        `json:"acknowledgments"`
		Canonical          []string        `json:"canonical"`
		Hiring             []string        `json:"hiring"`
		CSAF               []string        `json:"csaf"`
		PreferredLanguages []string        `json:"preferred_languages"`
		Fields             *int            `json:"fields"`
		CanonicalMatches   map[string]bool `json:"canonical_matches"`
	} `json:"cases"`
}

func sameList(a, b []string) bool { return strings.Join(a, "\x00") == strings.Join(b, "\x00") }

// TestWKConformanceSecurityTxt runs conformance/wellknown/securitytxt.json.
func TestWKConformanceSecurityTxt(t *testing.T) {
	var f securityFile
	if err := unmarshal(testkit.ReadFile(t, "wellknown", "securitytxt.json"), &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, c.Now)
			if err != nil {
				t.Fatal(err)
			}
			s := ParseSecurityTxt([]byte(c.Text), now, SecurityOptions{MaxItems: c.Options.MaxItems, MaxBytes: c.Options.MaxBytes})
			if s.Valid() != c.Valid || s.Signed != c.Signed {
				t.Errorf("valid %v signed %v, want %v %v (%v)", s.Valid(), s.Signed, c.Valid, c.Signed, s.Diags)
			}
			want := c.Grade
			if want == "" {
				want = "complete"
			}
			if s.Grade.String() != want {
				t.Errorf("grade %s, want %s", s.Grade, want)
			}
			var got []string
			for _, d := range s.Diags {
				got = append(got, d.Code.String())
			}
			if c.Diags != nil && !sameList(got, c.Diags) {
				t.Errorf("diags %v, want %v", got, c.Diags)
			}
			for _, chk := range []struct {
				name      string
				got, want []string
			}{
				{"contact", s.Contact, c.Contact}, {"encryption", s.Encryption, c.Encryption},
				{"policy", s.Policy, c.Policy}, {"acknowledgments", s.Acknowledgments, c.Acknowledgments},
				{"canonical", s.Canonical, c.Canonical}, {"hiring", s.Hiring, c.Hiring}, {"csaf", s.CSAF, c.CSAF},
				{"preferred_languages", s.PreferredLanguages, c.PreferredLanguages},
			} {
				if chk.want != nil && !sameList(chk.got, chk.want) {
					t.Errorf("%s %v, want %v", chk.name, chk.got, chk.want)
				}
			}
			if c.Expires != "" && (!s.HasExpires || s.Expires.Format("2006-01-02T15:04:05.000Z") != c.Expires) {
				t.Errorf("expires %v, want %s", s.Expires, c.Expires)
			}
			if c.Fields != nil && len(s.Fields) != *c.Fields {
				t.Errorf("%d fields, want %d", len(s.Fields), *c.Fields)
			}
			for u, w := range c.CanonicalMatches {
				if s.CanonicalMatches(u) != w {
					t.Errorf("CanonicalMatches(%q) = %v", u, !w)
				}
			}
		})
	}
	if len(f.Cases) < 15 {
		t.Fatalf("only %d security.txt cases", len(f.Cases))
	}
}
