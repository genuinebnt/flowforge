package cssparse

import (
	"fmt"
	"math"
)

// This file renders trees in the css-parsing-tests JSON representation
// (conformance/third_party/css-parsing-tests/README.rst) so the vendored
// suite and our own conformance data can be compared structurally.

func (d *Document) jsonList(first NodeID) []any {
	out := []any{}
	for c := first; c != NoNode; c = d.Nodes[c].NextSibling {
		out = append(out, d.jsonItems(c)...)
	}
	return out
}

// jsonItems returns the JSON items for one node: usually one, two when an
// unterminated string or url is followed by its EOF error marker.
func (d *Document) jsonItems(id NodeID) []any {
	n := &d.Nodes[id]
	switch n.Kind {
	case NodePreserved:
		return d.jsonToken(&d.Tokens[n.Token])
	case NodeSimpleBlock:
		open := d.Tokens[n.Token].Kind
		name := map[TokenKind]string{TokenOpenCurly: "{}", TokenOpenSquare: "[]", TokenOpenParen: "()"}[open]
		return []any{append([]any{name}, d.jsonList(n.FirstChild)...)}
	case NodeFunction:
		return []any{append([]any{"function", d.Name(id)}, d.jsonList(n.FirstChild)...)}
	case NodeDeclaration:
		return []any{[]any{"declaration", d.Name(id), d.jsonList(n.FirstChild), n.Important}}
	case NodeAtRule:
		var block any
		if n.Block != NoNode {
			block = d.jsonList(d.Nodes[n.Block].FirstChild)
		}
		return []any{[]any{"at-rule", d.Name(id), d.jsonList(n.FirstChild), block}}
	case NodeQualifiedRule:
		return []any{[]any{"qualified rule", d.jsonList(n.FirstChild), d.jsonList(d.Nodes[n.Block].FirstChild)}}
	case NodeError:
		switch n.Error {
		case DiagEmpty:
			return []any{[]any{"error", "empty"}}
		case DiagExtraInput:
			return []any{[]any{"error", "extra-input"}}
		case DiagDepthLimit:
			return []any{[]any{"error", "depth-limit"}}
		}
		return []any{[]any{"error", "invalid"}}
	}
	return []any{[]any{"node", n.Kind.String()}}
}

func numType(t *Token) string {
	if t.Flags&FlagInteger != 0 {
		return "integer"
	}
	return "number"
}

func (d *Document) jsonToken(t *Token) []any {
	repr := func() string { return d.Src[t.Start:t.ReprEnd] }
	switch t.Kind {
	case TokenIdent:
		return []any{[]any{"ident", t.Value}}
	case TokenAtKeyword:
		return []any{[]any{"at-keyword", t.Value}}
	case TokenHash:
		typ := "unrestricted"
		if t.Flags&FlagHashID != 0 {
			typ = "id"
		}
		return []any{[]any{"hash", t.Value, typ}}
	case TokenString:
		if t.Flags&FlagUnterminated != 0 {
			return []any{[]any{"string", t.Value}, []any{"error", "eof-in-string"}}
		}
		return []any{[]any{"string", t.Value}}
	case TokenBadString:
		return []any{[]any{"error", "bad-string"}}
	case TokenURL:
		if t.Flags&FlagUnterminated != 0 {
			return []any{[]any{"url", t.Value}, []any{"error", "eof-in-url"}}
		}
		return []any{[]any{"url", t.Value}}
	case TokenBadURL:
		return []any{[]any{"error", "bad-url"}}
	case TokenDelim:
		return []any{t.Value}
	case TokenNumber:
		return []any{[]any{"number", repr(), t.Number, numType(t)}}
	case TokenPercentage:
		return []any{[]any{"percentage", repr(), t.Number, numType(t)}}
	case TokenDimension:
		return []any{[]any{"dimension", repr(), t.Number, numType(t), t.Value}}
	case TokenUnicodeRange:
		return []any{[]any{"unicode-range", float64(t.RangeStart), float64(t.RangeEnd)}}
	case TokenWhitespace:
		return []any{" "}
	case TokenCloseCurly, TokenCloseSquare, TokenCloseParen:
		return []any{[]any{"error", fixedText[t.Kind]}}
	}
	return []any{fixedText[t.Kind]}
}

// jsonEqual compares decoded JSON values, numbers within a relative 1e-9.
func jsonEqual(a, b any) bool {
	switch x := a.(type) {
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case float64:
		y, ok := b.(float64)
		if !ok {
			return false
		}
		return x == y || math.Abs(x-y) <= 1e-9*math.Max(math.Abs(x), math.Abs(y))
	case nil:
		return b == nil
	default:
		return fmt.Sprint(a) == fmt.Sprint(b) && fmt.Sprintf("%T", a) == fmt.Sprintf("%T", b)
	}
}

// runParser runs the named "parse a ..." entry point and renders its
// result: a list for the list parsers, one item for the one-item parsers.
func runParser(name string, src []byte, opts Options) (any, *Document, error) {
	var d *Document
	one := false
	switch name {
	case "component_value_list":
		d = ParseComponentValues(src, opts)
	case "one_component_value":
		d, one = ParseComponentValue(src, opts), true
	case "declaration_list":
		d = ParseDeclarationList(src, opts)
	case "one_declaration":
		d, one = ParseDeclaration(src, opts), true
	case "blocks_contents":
		d = ParseBlockContentsSource(src, opts)
	case "rule_list":
		d = ParseRuleList(src, opts)
	case "one_rule":
		d, one = ParseRule(src, opts), true
	case "stylesheet":
		d = ParseStylesheet(src, opts)
	default:
		return nil, nil, fmt.Errorf("unknown parser %q", name)
	}
	items := d.jsonList(d.Nodes[d.Root].FirstChild)
	if one {
		if len(items) != 1 {
			return nil, d, fmt.Errorf("one-item parser produced %d items", len(items))
		}
		return items[0], d, nil
	}
	return items, d, nil
}
