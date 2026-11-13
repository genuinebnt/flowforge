package vm

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/bytecode"
)

// Display renders the display form used by INTERP and to_string, without
// module type names (user types render as T<id>). The VM itself renders
// with the module's names.
//
// PORT: display strings flow into effect arguments (alert messages, log
// lines), so they are part of the args-hash contract
// (docs/PORTING_RUST.md 5.13). Strings are raw at top level and
// strconv.Quote'd inside collections: Go escaping (\a \b \f \n \r \t \v,
// \xNN for invalid bytes, \uNNNN and \UNNNNNNNN for runes that are not
// unicode.IsPrint), unlike Rust's {:?} (\u{...}). Durations use
// time.Duration.String, times RFC3339Nano in UTC with trailing fractional
// zeros trimmed, floats FormatFloat. Port each algorithm exactly.
func Display(v Value) string {
	d := displayer{max: math.MaxInt}
	d.value(v, true)
	return d.sb.String()
}

// DisplayIn renders v with the type names of module m.
func DisplayIn(m *bytecode.Module, v Value) string {
	d := displayer{m: m, max: math.MaxInt}
	d.value(v, true)
	return d.sb.String()
}

// displayer writes into a builder and stops once max bytes are exceeded,
// so displaying a huge value costs at most O(max) work.
type displayer struct {
	m        *bytecode.Module
	sb       strings.Builder
	max      int
	overflow bool
}

func (d *displayer) write(s string) {
	if d.overflow {
		return
	}
	if d.sb.Len()+len(s) > d.max {
		d.overflow = true
		return
	}
	d.sb.WriteString(s)
}

func (d *displayer) layout(t bytecode.TypeID) *bytecode.TypeLayout {
	if d.m != nil {
		if l, ok := d.m.Layout(t); ok {
			return l
		}
	} else if l, ok := bytecode.BuiltinType(t); ok {
		return l
	}
	return nil
}

// FormatFloat renders a float so that it always reads as a float:
// strconv.FormatFloat(f, 'g', -1, 64), the shortest round-trip digits in
// exponent form when the decimal exponent is below -4 or at least 6
// ("1e+06", "1.234567e+06", "1e-05"; but "100000" and "0.0001"), plus ".0"
// when the result has no '.', 'e' or 'n' ("2" becomes "2.0").
func FormatFloat(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eEn") {
		s += ".0"
	}
	return s
}

// FormatTime renders Unix nanoseconds as RFC 3339 UTC with up to nine
// fractional digits, trailing zeros trimmed (time.RFC3339Nano):
// "1970-01-01T00:00:01.5Z", "1970-01-01T00:00:00Z".
func FormatTime(ns int64) string {
	return time.Unix(0, ns).UTC().Format(time.RFC3339Nano)
}

func (d *displayer) value(v Value, top bool) {
	if d.overflow {
		return
	}
	switch v.k {
	case KindUnit:
		d.write("()")
	case KindBool:
		d.write(strconv.FormatBool(v.AsBool()))
	case KindInt:
		d.write(strconv.FormatInt(v.AsInt(), 10))
	case KindDuration:
		d.write(time.Duration(v.AsInt()).String())
	case KindTime:
		d.write(FormatTime(v.AsInt()))
	case KindFloat:
		d.write(FormatFloat(v.AsFloat()))
	case KindString:
		if top {
			d.write(v.Str())
		} else {
			d.write(strconv.Quote(v.Str()))
		}
	case KindList:
		d.write("[")
		d.seq(v.Elems())
		d.write("]")
	case KindMap:
		m := v.p.(*mapObj)
		d.write("{")
		for i := range m.keys {
			if i > 0 {
				d.write(", ")
			}
			d.value(m.keys[i], false)
			d.write(": ")
			d.value(m.vals[i], false)
		}
		d.write("}")
	case KindRecord:
		t := v.TypeID()
		if t == bytecode.TypeTuple {
			d.write("(")
			d.seq(v.Fields())
			d.write(")")
			return
		}
		l := d.layout(t)
		if l == nil {
			d.write("T" + strconv.Itoa(int(t)))
		} else {
			d.write(l.Name)
		}
		d.write(" {")
		for i, f := range v.Fields() {
			if i > 0 {
				d.write(",")
			}
			d.write(" ")
			if l != nil && i < len(l.Fields) {
				d.write(l.Fields[i].Name)
			} else {
				d.write(strconv.Itoa(i))
			}
			d.write(": ")
			d.value(f, false)
		}
		d.write(" }")
	case KindEnum:
		t := v.TypeID()
		l := d.layout(t)
		if l != nil && int(v.Variant()) < len(l.Variants) {
			d.write(l.Variants[v.Variant()].Name)
		} else {
			d.write("T" + strconv.Itoa(int(t)) + "::" + strconv.Itoa(int(v.Variant())))
		}
		if fs := v.Fields(); len(fs) > 0 {
			d.write("(")
			d.seq(fs)
			d.write(")")
		}
	case KindClosure:
		d.write("<closure " + strconv.Itoa(int(v.FuncIndex())) + ">")
	case KindScopeProof:
		d.write("InScope(")
		d.value(v.ProofTarget(), false)
		d.write(")")
	default:
		d.write(v.Str())
	}
}

func (d *displayer) seq(vs []Value) {
	for i, e := range vs {
		if i > 0 {
			d.write(", ")
		}
		d.value(e, false)
		if d.overflow {
			return
		}
	}
}
