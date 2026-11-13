package textindex

import (
	"fmt"
	"slices"
)

// Schema declares the fields of an index. Every field name is unique
// across the four lists, is 1..64 bytes of [A-Za-z0-9_] starting with a
// letter or '_', and is not one of the reserved query names "since" and
// "until". The order of each list is significant: it fixes field ids in
// the segment format and the order in which per-field BM25 scores are
// summed.
type Schema struct {
	// Text fields are analyzed (positions stored), trigram-indexed for
	// regex queries and stored. Unqualified query clauses search all of
	// them.
	Text []string
	// Keyword fields hold one exact value per document (filters such as
	// program, kind, side, feature, mime). They are doc-values columns.
	Keyword []string
	// Numeric fields hold one int64 per document (dates as Unix ms).
	Numeric []string
	// Stored fields are stored only (returned by Get, never searched).
	Stored []string
}

// Field types, as Schema.lookup returns them.
const (
	fieldNone = iota
	fieldText
	fieldKeyword
	fieldNumeric
	fieldStored
)

// maxFieldsPerKind bounds each schema list (ids are one byte).
const maxFieldsPerKind = 64

// DefaultSchema is the diff-search schema of V2_DESIGN §7: text fields
// "text" (hunk or record text) and "url"; keyword fields program,
// url_template, kind, side, feature and mime; numeric field "at" (Unix ms).
func DefaultSchema() Schema {
	return Schema{
		Text:    []string{"text", "url"},
		Keyword: []string{"program", "url_template", "kind", "side", "feature", "mime"},
		Numeric: []string{"at"},
	}
}

// validFieldName is the naming rule of Schema.
func validFieldName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// Validate checks the naming rules above.
func (s Schema) Validate() error {
	seen := map[string]bool{}
	for _, l := range [][]string{s.Text, s.Keyword, s.Numeric, s.Stored} {
		if len(l) > maxFieldsPerKind {
			return fmt.Errorf("%w: more than %d fields of one kind", ErrInvalidSchema, maxFieldsPerKind)
		}
		for _, n := range l {
			if !validFieldName(n) {
				return fmt.Errorf("%w: bad field name %q", ErrInvalidSchema, n)
			}
			if n == "since" || n == "until" {
				return fmt.Errorf("%w: field name %q is reserved", ErrInvalidSchema, n)
			}
			if seen[n] {
				return fmt.Errorf("%w: duplicate field %q", ErrInvalidSchema, n)
			}
			seen[n] = true
		}
	}
	if len(s.Text) == 0 {
		return fmt.Errorf("%w: at least one text field is required", ErrInvalidSchema)
	}
	return nil
}

// Equal reports whether two schemas are identical (same lists, same order).
func (s Schema) Equal(o Schema) bool {
	return slices.Equal(s.Text, o.Text) && slices.Equal(s.Keyword, o.Keyword) &&
		slices.Equal(s.Numeric, o.Numeric) && slices.Equal(s.Stored, o.Stored)
}

// lookup returns the type and index of a field name.
func (s Schema) lookup(name string) (int, int) {
	if i := slices.Index(s.Text, name); i >= 0 {
		return fieldText, i
	}
	if i := slices.Index(s.Keyword, name); i >= 0 {
		return fieldKeyword, i
	}
	if i := slices.Index(s.Numeric, name); i >= 0 {
		return fieldNumeric, i
	}
	if i := slices.Index(s.Stored, name); i >= 0 {
		return fieldStored, i
	}
	return fieldNone, -1
}

// FieldValue is one named string value.
type FieldValue struct {
	Name  string
	Value string
}

// NumericValue is one named integer value.
type NumericValue struct {
	Name  string
	Value int64
}

// Document is the unit of indexing. ID is caller-assigned and unique among
// live documents. Each field appears at most once. Empty string values are
// treated as absent (dropped by normalization).
type Document struct {
	ID      uint64
	Text    []FieldValue
	Keyword []FieldValue
	Numeric []NumericValue
	Stored  []FieldValue
}

// docFields is a document in schema order: one slot per schema field.
// Missing string fields are "", missing numerics have has=false.
type docFields struct {
	id      uint64
	gen     uint64
	text    []string
	keyword []string
	numeric []int64
	hasNum  []bool
	stored  []string
}

// normalize validates d against s and returns it in schema order.
func normalize(s Schema, d Document) (docFields, error) {
	f := docFields{
		id:      d.ID,
		text:    make([]string, len(s.Text)),
		keyword: make([]string, len(s.Keyword)),
		numeric: make([]int64, len(s.Numeric)),
		hasNum:  make([]bool, len(s.Numeric)),
		stored:  make([]string, len(s.Stored)),
	}
	setStr := func(kind string, list []string, dst []string, vals []FieldValue) error {
		seen := make([]bool, len(list))
		for _, v := range vals {
			i := slices.Index(list, v.Name)
			if i < 0 {
				return fmt.Errorf("%w: document %d: %s field %q not in schema", ErrInvalidDocument, d.ID, kind, v.Name)
			}
			if seen[i] {
				return fmt.Errorf("%w: document %d: field %q repeated", ErrInvalidDocument, d.ID, v.Name)
			}
			seen[i] = true
			dst[i] = v.Value
		}
		return nil
	}
	if err := setStr("text", s.Text, f.text, d.Text); err != nil {
		return f, err
	}
	if err := setStr("keyword", s.Keyword, f.keyword, d.Keyword); err != nil {
		return f, err
	}
	if err := setStr("stored", s.Stored, f.stored, d.Stored); err != nil {
		return f, err
	}
	for _, v := range d.Numeric {
		i := slices.Index(s.Numeric, v.Name)
		if i < 0 {
			return f, fmt.Errorf("%w: document %d: numeric field %q not in schema", ErrInvalidDocument, d.ID, v.Name)
		}
		if f.hasNum[i] {
			return f, fmt.Errorf("%w: document %d: field %q repeated", ErrInvalidDocument, d.ID, v.Name)
		}
		f.hasNum[i] = true
		f.numeric[i] = v.Value
	}
	return f, nil
}

// document converts back to the public form, fields in schema order,
// absent fields omitted.
func (f *docFields) document(s Schema) Document {
	d := Document{ID: f.id}
	for i, v := range f.text {
		if v != "" {
			d.Text = append(d.Text, FieldValue{s.Text[i], v})
		}
	}
	for i, v := range f.keyword {
		if v != "" {
			d.Keyword = append(d.Keyword, FieldValue{s.Keyword[i], v})
		}
	}
	for i, ok := range f.hasNum {
		if ok {
			d.Numeric = append(d.Numeric, NumericValue{s.Numeric[i], f.numeric[i]})
		}
	}
	for i, v := range f.stored {
		if v != "" {
			d.Stored = append(d.Stored, FieldValue{s.Stored[i], v})
		}
	}
	return d
}

// Normalize returns d validated against s with fields in schema order and
// empty values dropped: the form Get returns.
func Normalize(s Schema, d Document) (Document, error) {
	f, err := normalize(s, d)
	if err != nil {
		return Document{}, err
	}
	return f.document(s), nil
}
