package engine

import (
	"strings"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/vm"
)

// builtinRecord builds a value of a built-in record type (ScopeDenied,
// Approval, Denied, ...) by field name, following the layout in
// bytecode.BuiltinType. Fields missing from vals get the zero value of
// their declared type, so the engine keeps producing well-formed values
// if a layout gains a field.
func builtinRecord(id bytecode.TypeID, vals map[string]vm.Value) vm.Value {
	l, ok := bytecode.BuiltinType(id)
	if !ok {
		return vm.Unit()
	}
	fields := make([]vm.Value, len(l.Fields))
	for i, f := range l.Fields {
		if v, ok := vals[f.Name]; ok {
			fields[i] = v
			continue
		}
		fields[i] = zeroOf(f.Type)
	}
	return vm.Record(id, fields...)
}

// zeroOf returns a default value for a DSL type text.
func zeroOf(typ string) vm.Value {
	switch {
	case strings.HasPrefix(typ, "Option<"):
		return vm.None()
	case strings.HasPrefix(typ, "List<"), strings.HasPrefix(typ, "Stream<"):
		return vm.List()
	}
	switch typ {
	case "String":
		return vm.Str("")
	case "Int":
		return vm.Int(0)
	case "Float":
		return vm.Float(0)
	case "Bool":
		return vm.Bool(false)
	case "Time":
		return vm.Time(0)
	case "Duration":
		return vm.Duration(0)
	}
	return vm.Unit()
}

func optStr(s string) vm.Value {
	if s == "" {
		return vm.None()
	}
	return vm.Some(vm.Str(s))
}
