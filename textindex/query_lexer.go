package textindex

// Query lexer: an explicit state machine over the query bytes (README
// "Query language"). It produces the whole token list up front.

// Query-language limits.
const (
	MaxQueryBytes   = 8192 // longer queries fail with QUERY_TOO_LONG
	MaxQueryDepth   = 64   // nested groups and negations
	MaxQueryClauses = 1024 // leaves (terms, phrases, regexes, time filters)
)

type tokKind uint8

const (
	tokEOF    tokKind = iota
	tokLParen         // "("
	tokRParen         // ")"
	tokMinus          // "-" at the start of a clause
	tokAnd            // bare word "AND"
	tokOr             // bare word "OR"
	tokWord           // bare word
	tokPhrase         // "..." (text is unescaped)
	tokRegex          // /.../flags (text is the pattern, flags canonical)
	tokField          // identifier immediately followed by ':' (text is the name)
)

func (k tokKind) String() string {
	return [...]string{"end of query", "'('", "')'", "'-'", "AND", "OR", "term", "phrase", "regex", "field"}[k]
}

type qtoken struct {
	kind       tokKind
	start, end int
	text       string
	flags      string
	// value is set on a token that directly follows a tokField (no
	// whitespace): it is the field's value.
	value bool
}

// lexState is the state of the query lexer.
type lexState uint8

const (
	// lexStart: between tokens; whitespace is skipped.
	lexStart lexState = iota
	// lexValue: right after "field:"; a value must follow immediately.
	lexValue
	// lexWord: inside a bare word.
	lexWord
	// lexValueWord: inside a field value word (':' and a leading '-' are
	// ordinary bytes).
	lexValueWord
	// lexPhrase: inside "...".
	lexPhrase
	// lexPhraseEscape: after '\' inside a phrase.
	lexPhraseEscape
	// lexRegex: inside /.../.
	lexRegex
	// lexRegexEscape: after '\' inside a regex.
	lexRegexEscape
	// lexRegexFlags: after the closing '/' of a regex.
	lexRegexFlags
)

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// wordEnd reports bytes that end a bare word.
func wordEnd(c byte) bool { return isSpace(c) || c == '(' || c == ')' || c == '"' }

func isIdentStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isIdentByte(c byte) bool  { return isIdentStart(c) || c >= '0' && c <= '9' }

// lexQuery tokenizes q. It returns the tokens (ending with tokEOF) or the
// first lexical error.
func lexQuery(q string) ([]qtoken, *QueryError) {
	var toks []qtoken
	state := lexStart
	start := 0            // start of the current token
	ident := true         // lexWord: the bytes so far form an identifier
	valueTok := false     // the token being lexed is a field value
	var buf []byte        // unescaped phrase or regex text
	var flagsSeen [3]bool // i, m, s
	emit := func(k tokKind, end int, text string) {
		toks = append(toks, qtoken{kind: k, start: start, end: end, text: text, value: valueTok})
		valueTok = false
	}
	i := 0
	for i <= len(q) {
		eof := i == len(q)
		var c byte
		if !eof {
			c = q[i]
		}
		switch state {
		case lexStart:
			switch {
			case eof:
				i++
			case isSpace(c):
				i++
			case c == '(':
				start = i
				emit(tokLParen, i+1, "")
				i++
			case c == ')':
				start = i
				emit(tokRParen, i+1, "")
				i++
			case c == '-':
				start = i
				emit(tokMinus, i+1, "")
				i++
			case c == '"':
				start, buf, state = i, buf[:0], lexPhrase
				i++
			case c == '/':
				start, buf, state = i, buf[:0], lexRegex
				i++
			default:
				start, ident, state = i, isIdentStart(c), lexWord
			}
		case lexValue:
			valueTok = true
			switch {
			case eof || isSpace(c) || c == '(' || c == ')':
				end := i
				if !eof {
					end = i + 1
				}
				return nil, qerr(CodeExpectedValue, i, end, "field %q needs a value right after ':'", toks[len(toks)-1].text)
			case c == '"':
				start, buf, state = i, buf[:0], lexPhrase
				i++
			case c == '/':
				start, buf, state = i, buf[:0], lexRegex
				i++
			default:
				start, state = i, lexValueWord
			}
		case lexWord:
			switch {
			case eof || wordEnd(c):
				w := q[start:i]
				switch w {
				case "AND":
					emit(tokAnd, i, w)
				case "OR":
					emit(tokOr, i, w)
				default:
					emit(tokWord, i, w)
				}
				state = lexStart
			case c == ':' && ident && i > start:
				emit(tokField, i+1, q[start:i])
				state = lexValue
				i++
			default:
				if !isIdentByte(c) {
					ident = false
				}
				i++
			}
		case lexValueWord:
			if eof || wordEnd(c) {
				valueTok = true
				emit(tokWord, i, q[start:i])
				state = lexStart
			} else {
				i++
			}
		case lexPhrase:
			switch {
			case eof:
				return nil, qerr(CodeUnterminatedPhrase, start, len(q), "phrase is not closed with '\"'")
			case c == '\\':
				state = lexPhraseEscape
			case c == '"':
				emit(tokPhrase, i+1, string(buf))
				state = lexStart
			default:
				buf = append(buf, c)
			}
			i++
		case lexPhraseEscape:
			if eof {
				return nil, qerr(CodeUnterminatedPhrase, start, len(q), "phrase is not closed with '\"'")
			}
			buf = append(buf, c)
			state = lexPhrase
			i++
		case lexRegex:
			switch {
			case eof:
				return nil, qerr(CodeUnterminatedRegex, start, len(q), "regex is not closed with '/'")
			case c == '\\':
				state = lexRegexEscape
			case c == '/':
				if len(buf) == 0 {
					return nil, qerr(CodeEmptyRegex, start, i+1, "empty regex")
				}
				flagsSeen = [3]bool{}
				state = lexRegexFlags
			default:
				buf = append(buf, c)
			}
			i++
		case lexRegexEscape:
			if eof {
				return nil, qerr(CodeUnterminatedRegex, start, len(q), "regex is not closed with '/'")
			}
			if c != '/' {
				buf = append(buf, '\\')
			}
			buf = append(buf, c)
			state = lexRegex
			i++
		case lexRegexFlags:
			if !eof && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
				k := -1
				switch c {
				case 'i':
					k = 0
				case 'm':
					k = 1
				case 's':
					k = 2
				}
				if k < 0 || flagsSeen[k] {
					return nil, qerr(CodeBadRegexFlag, i, i+1, "regex flag %q is unknown or repeated (allowed: i, m, s)", string(c))
				}
				flagsSeen[k] = true
				i++
				continue
			}
			var fl []byte
			for k, f := range "ims" {
				if flagsSeen[k] {
					fl = append(fl, byte(f))
				}
			}
			emit(tokRegex, i, string(buf))
			toks[len(toks)-1].flags = string(fl)
			state = lexStart
		}
	}
	toks = append(toks, qtoken{kind: tokEOF, start: len(q), end: len(q)})
	return toks, nil
}
