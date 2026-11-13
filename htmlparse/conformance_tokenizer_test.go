package htmlparse

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
)

// html5lib tokenizer test format (conformance/third_party/html5lib-tests/
// tokenizer/README.md).
type h5TokFile struct {
	Tests         []h5TokTest `json:"tests"`
	XMLViolations []h5TokTest `json:"xmlViolationTests"`
}

type h5TokTest struct {
	Description   string            `json:"description"`
	Input         json.RawMessage   `json:"input"`
	Output        []json.RawMessage `json:"output"`
	InitialStates []string          `json:"initialStates"`
	LastStartTag  string            `json:"lastStartTag"`
	DoubleEscaped bool              `json:"doubleEscaped"`
	Errors        []struct {
		Code string `json:"code"`
		Line int    `json:"line"`
		Col  int    `json:"col"`
	} `json:"errors"`
}

var h5InitialStates = map[string]TokenizerState{
	"Data state":          DataState,
	"PLAINTEXT state":     PLAINTEXTState,
	"RCDATA state":        RCDATAState,
	"RAWTEXT state":       RAWTEXTState,
	"Script data state":   ScriptDataState,
	"CDATA section state": CDATASectionState,
}

// knownTokenizerErrorPositionMismatches lists html5lib tokenizer cases
// whose tokens and error codes match but whose expected error positions
// disagree with the standard's state machine. See OPEN_QUESTIONS.md
// (Q-TOK-1). The test fails if this list is not exact.
var knownTokenizerErrorPositionMismatches = []string{
	"test3.test#1167[Data state] <?a a",
	"test3.test#1168[Data state] <?b b",
	"test3.test#1169[Data state] <?y y",
	"test3.test#1170[Data state] <?z z",
	"test3.test#1171[Data state] <?A A",
	"test3.test#1172[Data state] <?B B",
	"test3.test#1173[Data state] <?Y Y",
	"test3.test#1174[Data state] <?Z Z?",
	"test3.test#1175[Data state] <?a a?",
	"test3.test#1176[Data state] <?b b?",
	"test3.test#1177[Data state] <?y y?",
	"test3.test#1178[Data state] <?z z?",
	"test3.test#1179[Data state] <?A A?",
	"test3.test#1180[Data state] <?B B?",
	"test3.test#1181[Data state] <?Y Y?",
	"test3.test#1182[Data state] <?Z Z?",
}

// tokenizerCaseResult is one case's outcome.
type tokenizerCaseResult struct {
	id                 string
	tokensOK, errorsOK bool
	codesOK            bool
	detail             string
}

func loadTokenizerSuite(t testing.TB) map[string]h5TokFile {
	t.Helper()
	dir := testkit.ConformanceDir(t, "third_party/html5lib-tests/tokenizer")
	names, err := filepath.Glob(filepath.Join(dir, "*.test"))
	if err != nil || len(names) == 0 {
		t.Fatalf("no tokenizer tests in %s: %v", dir, err)
	}
	out := map[string]h5TokFile{}
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		var f h5TokFile
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		out[filepath.Base(n)] = f
	}
	return out
}

// TestHTMLConformanceTokenizer runs the html5lib tokenizer suite under
// every named character reference strategy (each must pass it alike).
func TestHTMLConformanceTokenizer(t *testing.T) {
	suite := loadTokenizerSuite(t)
	for _, st := range CharRefStrategies() {
		t.Run(st.String(), func(t *testing.T) { tokenizerSuite(t, suite, st) })
	}
}

func tokenizerSuite(t *testing.T, suite map[string]h5TokFile, cr CharRefStrategy) {
	files := make([]string, 0, len(suite))
	for f := range suite {
		files = append(files, f)
	}
	sort.Strings(files)
	var total, passTokens, passErrors int
	var tokenFailures, posMismatches, posDetails []string
	for _, f := range files {
		tests := suite[f].Tests
		xml := false
		if len(suite[f].XMLViolations) > 0 {
			tests, xml = suite[f].XMLViolations, true
		}
		for i, tc := range tests {
			states := tc.InitialStates
			if len(states) == 0 {
				states = []string{"Data state"}
			}
			for _, st := range states {
				total++
				r := runTokenizerCase(t, f, i, tc, st, xml, cr)
				if r.tokensOK {
					passTokens++
				} else {
					tokenFailures = append(tokenFailures, r.id+": "+r.detail)
				}
				switch {
				case r.errorsOK:
					passErrors++
				case r.codesOK:
					posMismatches = append(posMismatches, r.id)
					posDetails = append(posDetails, r.id+": "+r.detail)
				default:
					tokenFailures = append(tokenFailures, r.id+": errors: "+r.detail)
				}
			}
		}
	}
	t.Logf("html5lib tokenizer: %d runs, tokens %d/%d, errors %d/%d (%d position-only mismatches)",
		total, passTokens, total, passErrors, total, len(posMismatches))
	if len(tokenFailures) > 0 {
		for _, f := range tokenFailures[:min(len(tokenFailures), 40)] {
			t.Error(f)
		}
		t.Fatalf("%d tokenizer failures", len(tokenFailures))
	}
	if !slices.Equal(posMismatches, knownTokenizerErrorPositionMismatches) {
		t.Errorf("error-position mismatch list changed; got %d entries:\n%s", len(posMismatches), quoteList(posMismatches))
		if os.Getenv("FF_HTML_VERBOSE") != "" {
			for _, d := range posDetails {
				t.Log(d)
			}
		}
	}
}

func quoteList(ss []string) string {
	var b strings.Builder
	for _, s := range ss {
		fmt.Fprintf(&b, "\t%q,\n", s)
	}
	return b.String()
}

func runTokenizerCase(t *testing.T, file string, idx int, tc h5TokTest, st string, xml bool, cr CharRefStrategy) tokenizerCaseResult {
	id := fmt.Sprintf("%s#%d[%s] %s", file, idx, st, tc.Description)
	res := tokenizerCaseResult{id: id}
	input, err := decodeJSONStringWTF8(tc.Input)
	if err != nil {
		t.Fatalf("%s: input: %v", id, err)
	}
	if tc.DoubleEscaped {
		input, _ = unescapeU(input, false)
	}
	state, ok := h5InitialStates[st]
	if !ok {
		t.Fatalf("%s: unknown state %q", id, st)
	}
	toks, errs := Tokenize(input, TokenizeOptions{InitialState: state, LastStartTag: tc.LastStartTag, CharRefLookup: cr})
	got := tokensToH5(toks, xml)
	want := expectedH5(t, id, tc.Output, tc.DoubleEscaped)
	res.tokensOK = reflect.DeepEqual(got, want)
	if !res.tokensOK {
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want)
		res.detail = fmt.Sprintf("input %q\n got  %s\n want %s", input, gj, wj)
	}
	var gotCodes, wantCodes, gotPos, wantPos []string
	for _, e := range errs {
		gotCodes = append(gotCodes, e.Code.String())
		gotPos = append(gotPos, fmt.Sprintf("%s@%d:%d", e.Code, e.Line, e.Col))
	}
	for _, e := range tc.Errors {
		wantCodes = append(wantCodes, e.Code)
		wantPos = append(wantPos, fmt.Sprintf("%s@%d:%d", e.Code, e.Line, e.Col))
	}
	if xml {
		// The xmlViolation tests describe the coerced infoset and list no
		// errors; the parse errors of the HTML input are not part of them.
		gotCodes, gotPos = nil, nil
	}
	res.codesOK = slices.Equal(gotCodes, wantCodes)
	res.errorsOK = res.codesOK && slices.Equal(gotPos, wantPos)
	if !res.errorsOK && res.tokensOK {
		res.detail = fmt.Sprintf("input %q\n got  %v\n want %v", input, gotPos, wantPos)
	}
	return res
}

// tokensToH5 converts tokens to the html5lib JSON shape with adjacent
// character tokens coalesced.
func tokensToH5(toks []Token, xml bool) []any {
	out := []any{}
	for _, tk := range toks {
		switch tk.Kind {
		case CharacterToken:
			d := tk.Data
			if xml {
				d = coerceInfosetText(d)
			}
			if n := len(out); n > 0 {
				if prev, ok := out[n-1].([]any); ok && prev[0] == "Character" {
					prev[1] = prev[1].(string) + d
					continue
				}
			}
			out = append(out, []any{"Character", d})
		case StartTagToken:
			attrs := map[string]any{}
			for _, a := range tk.Attrs {
				attrs[a.Name] = a.Value
			}
			e := []any{"StartTag", tk.Name, attrs}
			if tk.SelfClosing {
				e = append(e, true)
			}
			out = append(out, e)
		case EndTagToken:
			out = append(out, []any{"EndTag", tk.Name})
		case CommentToken:
			d := tk.Data
			if xml {
				d = coerceInfosetComment(d)
			}
			out = append(out, []any{"Comment", d})
		case DoctypeToken:
			opt := func(has bool, s string) any {
				if !has {
					return nil
				}
				return s
			}
			out = append(out, []any{"DOCTYPE", opt(tk.HasName, tk.Name), opt(tk.HasPublicID, tk.PublicID), opt(tk.HasSystemID, tk.SystemID), !tk.ForceQuirks})
		case ProcessingInstructionToken:
			out = append(out, []any{"ProcessingInstruction", tk.Name, tk.Data})
		}
	}
	return out
}

// coerceInfosetText and coerceInfosetComment apply the "Coercing an HTML
// DOM into an infoset" tweaks the xmlViolation tests expect (HTML
// §13.2.9): U+000C becomes a space, U+FFFF-style noncharacters become
// U+FFFD, "--" in comments becomes "- -" and a trailing "-" gets a space.
func coerceInfosetText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\f':
			b.WriteByte(' ')
		case isNoncharacter(r):
			b.WriteRune(0xFFFD)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func coerceInfosetComment(s string) string {
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "- -")
	}
	if strings.HasSuffix(s, "-") {
		s += " "
	}
	return s
}

// expectedH5 decodes the expected output with surrogates mapped to U+FFFD
// (the tokenizer's output policy) and the doubleEscaped second pass.
func expectedH5(t *testing.T, id string, raw []json.RawMessage, double bool) []any {
	out := []any{}
	for _, r := range raw {
		var arr []json.RawMessage
		if err := json.Unmarshal(r, &arr); err != nil {
			t.Fatalf("%s: output: %v", id, err)
		}
		e := make([]any, 0, len(arr))
		for _, x := range arr {
			e = append(e, decodeH5Value(t, id, x, double))
		}
		if e[0] == "Character" && len(out) > 0 {
			if prev, ok := out[len(out)-1].([]any); ok && prev[0] == "Character" {
				prev[1] = prev[1].(string) + e[1].(string)
				continue
			}
		}
		// html5lib DOCTYPE/StartTag shapes are used verbatim.
		out = append(out, e)
	}
	return out
}

func decodeH5Value(t *testing.T, id string, x json.RawMessage, double bool) any {
	s := strings.TrimSpace(string(x))
	switch {
	case s == "null":
		return nil
	case s == "true":
		return true
	case s == "false":
		return false
	case strings.HasPrefix(s, "\""):
		b, err := decodeJSONStringWTF8([]byte(s))
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if double {
			b, _ = unescapeU(b, false)
		}
		return replaceSurrogates(b)
	case strings.HasPrefix(s, "{"):
		var m map[string]json.RawMessage
		if err := json.Unmarshal(x, &m); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		out := map[string]any{}
		for k, v := range m {
			kb, _ := decodeJSONStringWTF8(mustJSON(k))
			if double {
				kb, _ = unescapeU(kb, false)
			}
			out[replaceSurrogates(kb)] = decodeH5Value(t, id, v, double)
		}
		return out
	}
	t.Fatalf("%s: unexpected JSON value %s", id, s)
	return nil
}

func mustJSON(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}
