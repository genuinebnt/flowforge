package graphstore

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxSearchText bounds the indexed display text of one entity (bytes).
const MaxSearchText = 2048

// searchSkip lists content fields never indexed: bulk registry text and
// credentials (URL username and password).
var searchSkip = map[string]bool{"raw": true, "password": true, "username": true}

// SearchText returns the lower-cased display text indexed for an entity
// (graph.entities.search_text, spec 10.5, 10.7): its top-level string and
// number content values in field order (names, addresses, URLs, banners,
// titles), de-duplicated, space-separated and capped at MaxSearchText
// bytes. Hash-like keys and nested values are not indexed.
func SearchText(content map[string]any) string {
	var parts []string
	seen := map[string]bool{}
	for _, k := range slices.Sorted(maps.Keys(content)) {
		if searchSkip[k] {
			continue
		}
		var s string
		switch v := content[k].(type) {
		case string:
			s = v
		case json.Number:
			s = v.String()
		default:
			continue
		}
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		parts = append(parts, s)
	}
	return truncateUTF8(strings.Join(parts, " "), MaxSearchText)
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// NormalizeQuery lower-cases and trims a search query.
func NormalizeQuery(q string) string { return strings.ToLower(strings.TrimSpace(q)) }

// Trigrams returns the sorted trigram set of s exactly as pg_trgm's
// show_trgm extracts it: s is lower-cased and split into words of letters
// and digits; each word is padded with two spaces in front and one behind
// and contributes every run of three consecutive characters.
func Trigrams(s string) []string {
	set := map[string]struct{}{}
	word := make([]rune, 0, 32)
	flush := func() {
		if len(word) == 0 {
			return
		}
		padded := append([]rune{' ', ' '}, word...)
		padded = append(padded, ' ')
		for i := 0; i+3 <= len(padded); i++ {
			set[string(padded[i:i+3])] = struct{}{}
		}
		word = word[:0]
	}
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			word = append(word, r)
			continue
		}
		flush()
	}
	flush()
	return slices.Sorted(maps.Keys(set))
}

// Similarity is pg_trgm's similarity(): shared trigrams over the size of
// the union of the two sets (0 when either is empty). It also returns the
// shared trigrams, sorted.
func Similarity(query, text []string) (float64, []string) {
	if len(query) == 0 || len(text) == 0 {
		return 0, nil
	}
	var shared []string
	i, j := 0, 0
	for i < len(query) && j < len(text) {
		switch c := cmp.Compare(query[i], text[j]); {
		case c == 0:
			shared = append(shared, query[i])
			i++
			j++
		case c < 0:
			i++
		default:
			j++
		}
	}
	n := len(shared)
	return float64(n) / float64(len(query)+len(text)-n), shared
}

// MatchSearch decides whether text (an entity's SearchText) matches the
// normalized query q (spec 5.9 search, 10.7): an exact substring always
// matches; with fuzzy, so does trigram similarity of at least threshold
// (typo tolerance). Without fuzzy every hit is verified by the exact
// substring test, which is what makes trigram candidates exact for
// queries of three characters or more and is a plain substring scan
// below that. score is the trigram similarity used for ranking.
func MatchSearch(q, text string, fuzzy bool, threshold float64) (score float64, matched []string, ok bool) {
	score, matched = Similarity(Trigrams(q), Trigrams(text))
	if strings.Contains(text, q) {
		return score, matched, true
	}
	if fuzzy && score >= threshold {
		return score, matched, true
	}
	return 0, nil, false
}

// SortHits orders hits by descending score, then entity id.
func SortHits(hits []SearchHit) {
	slices.SortFunc(hits, func(a, b SearchHit) int {
		return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.Entity.ID, b.Entity.ID))
	})
}

// ValidateSearch checks req and returns the normalized query and limit.
func ValidateSearch(req SearchRequest) (string, int, error) {
	q := NormalizeQuery(req.Query)
	if q == "" {
		return "", 0, InvalidArgument("query", "query is required")
	}
	if len(q) > 256 {
		return "", 0, InvalidArgument("query", "query longer than 256 bytes")
	}
	limit := req.Limit
	switch {
	case limit == 0:
		limit = DefaultSearchLimit
	case limit < 0 || limit > MaxSearchLimit:
		return "", 0, InvalidArgument("limit", "limit %d not in 1..%d", limit, MaxSearchLimit)
	}
	return q, limit, nil
}
