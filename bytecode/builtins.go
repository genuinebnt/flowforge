package bytecode

// BuiltinID is the id operand of BUILTIN. Ids are stable: new builtins are
// appended, existing ids never change meaning.
type BuiltinID uint16

// Builtin describes one pure native function callable with BUILTIN.
type Builtin struct {
	ID    BuiltinID
	Name  string
	Arity int
	// Doc is a one-line signature and behaviour summary.
	Doc string
}

// Builtin ids. Higher-order standard-library functions (map, filter, fold,
// any, all, first with a predicate, count with a predicate, sort_by,
// group_by) are lowered to loops by the compiler, so no builtin ever calls
// a closure; the keyed variants below take precomputed keys instead.
const (
	BLen BuiltinID = iota
	BContains
	BStartsWith
	BEndsWith
	BSplit
	BJoin
	BTrim
	BLower
	BUpper
	BReplace
	BParseInt
	BParseFloat
	BToString
	BTake
	BSkip
	BReverse
	BConcat
	BZip
	BEnumerate
	BDedupe
	BSort
	BSortByKeys
	BGroupByKeys
	BMapGet
	BMapInsert
	BMapKeys
	BMapValues
	BListGetOpt
	BRange
	BFqdnParse
	BIPParse
	BURLParse
	BCIDRParse
	BASNParse
	BHostParse
	BScopeAllows
	BFlatten
	BIntToFloat
	BFloatToInt
	BListAppend
	BBase64Decode
	BBase64Encode
	BURLDecode
	BURLEncode
	BHexDecode
	BHexEncode

	numBuiltins
)

var builtins = [numBuiltins]Builtin{
	BLen:         {BLen, "len", 1, "len(String | List | Map) -> Int; strings count Unicode scalar values"},
	BContains:    {BContains, "contains", 2, "contains(String, String) substring; contains(List<A>, A) structural; contains(Map<K,V>, K) key"},
	BStartsWith:  {BStartsWith, "starts_with", 2, "starts_with(String, String) -> Bool"},
	BEndsWith:    {BEndsWith, "ends_with", 2, "ends_with(String, String) -> Bool"},
	BSplit:       {BSplit, "split", 2, "split(String, sep String) -> List<String>; empty sep splits into scalar values"},
	BJoin:        {BJoin, "join", 2, "join(List<String>, sep String) -> String"},
	BTrim:        {BTrim, "trim", 1, "trim(String) -> String; Unicode white space"},
	BLower:       {BLower, "lower", 1, "lower(String) -> String; Unicode simple case mapping, locale independent"},
	BUpper:       {BUpper, "upper", 1, "upper(String) -> String"},
	BReplace:     {BReplace, "replace", 3, "replace(String, from, to) -> String; all non-overlapping occurrences"},
	BParseInt:    {BParseInt, "parse_int", 1, "parse_int(String) -> Option<Int>; optional + or - sign, then decimal digits only (no _, spaces or base prefix); None when out of i64 range"},
	BParseFloat:  {BParseFloat, "parse_float", 1, "parse_float(String) -> Option<Float>; finite values only"},
	BToString:    {BToString, "to_string", 1, "to_string(A) -> String; the display form INTERP uses"},
	BTake:        {BTake, "take", 2, "take(List<A>, n Int) -> List<A>; n < 0 is treated as 0"},
	BSkip:        {BSkip, "skip", 2, "skip(List<A>, n Int) -> List<A>"},
	BReverse:     {BReverse, "reverse", 1, "reverse(List<A>) -> List<A>"},
	BConcat:      {BConcat, "concat", 2, "concat(List<A>, List<A>) -> List<A>; also String, String"},
	BZip:         {BZip, "zip", 2, "zip(List<A>, List<B>) -> List<(A, B)>; stops at the shorter list"},
	BEnumerate:   {BEnumerate, "enumerate", 1, "enumerate(List<A>) -> List<(Int, A)>"},
	BDedupe:      {BDedupe, "dedupe", 1, "dedupe(List<A>) -> List<A>; keeps first occurrences, structural equality"},
	BSort:        {BSort, "sort", 1, "sort(List<A>) -> List<A>; stable, ascending structural order"},
	BSortByKeys:  {BSortByKeys, "sort_by_keys", 2, "sort_by_keys(keys List<K>, xs List<A>) -> List<A>; stable by key (lowering of sort_by)"},
	BGroupByKeys: {BGroupByKeys, "group_by_keys", 2, "group_by_keys(keys List<K>, xs List<A>) -> Map<K, List<A>>; groups in first-key order (lowering of group_by)"},
	BMapGet:      {BMapGet, "map_get", 2, "map_get(Map<K,V>, K) -> Option<V>"},
	BMapInsert:   {BMapInsert, "map_insert", 3, "map_insert(Map<K,V>, K, V) -> Map<K,V>; replaces in place, new keys go last"},
	BMapKeys:     {BMapKeys, "map_keys", 1, "map_keys(Map<K,V>) -> List<K>; insertion order"},
	BMapValues:   {BMapValues, "map_values", 1, "map_values(Map<K,V>) -> List<V>; insertion order"},
	BListGetOpt:  {BListGetOpt, "list_get_opt", 2, "list_get_opt(List<A>, i Int) -> Option<A>; first(xs) is list_get_opt(xs, 0)"},
	BRange:       {BRange, "range", 2, "range(a Int, b Int) -> List<Int>; half-open a..b, empty when b <= a"},
	BFqdnParse:   {BFqdnParse, "fqdn_parse", 1, "fqdn_parse(String) -> Option<Fqdn>; spec 4.2 normalization"},
	BIPParse:     {BIPParse, "ip_parse", 1, "ip_parse(String) -> Option<IpAddress>"},
	BURLParse:    {BURLParse, "url_parse", 1, "url_parse(String) -> Option<Url>"},
	BCIDRParse:   {BCIDRParse, "cidr_parse", 1, "cidr_parse(String) -> Option<Netblock>"},
	BASNParse:    {BASNParse, "asn_parse", 1, "asn_parse(String) -> Option<Asn>; \"AS64500\" or \"64500\""},
	BHostParse:   {BHostParse, "host_parse", 1, "host_parse(String) -> Option<Host>"},
	BScopeAllows: {BScopeAllows, "scope_allows", 1, "scope_allows(T) -> Bool; passive pre-filter against the pinned scope snapshot given to the VM (false without one)"},
	BFlatten:     {BFlatten, "flatten", 1, "flatten(List<List<A>>) -> List<A>"},
	BIntToFloat:  {BIntToFloat, "int_to_float", 1, "int_to_float(Int) -> Float"},
	BFloatToInt:  {BFloatToInt, "float_to_int", 1, "float_to_int(Float) -> Int; truncates toward zero, arith_overflow when out of range"},
	BListAppend:  {BListAppend, "list_append", 2, "list_append(List<A>, A) -> List<A>"},
	// The codec builtins (codec package): decoders return None for invalid
	// input and for bytes that are not UTF-8, since DSL strings are text.
	BBase64Decode: {BBase64Decode, "base64_decode", 1, "base64_decode(String) -> Option<String>; RFC 4648 lenient: either alphabet, padding optional, ASCII white space skipped"},
	BBase64Encode: {BBase64Encode, "base64_encode", 1, "base64_encode(String) -> String; RFC 4648 §4 standard alphabet, padded"},
	BURLDecode:    {BURLDecode, "url_decode", 1, "url_decode(String) -> Option<String>; percent-decoding (RFC 3986 §2.1), None on a malformed escape; '+' stays '+'"},
	BURLEncode:    {BURLEncode, "url_encode", 1, "url_encode(String) -> String; WHATWG component percent-encode set (encodeURIComponent), upper-case hex"},
	BHexDecode:    {BHexDecode, "hex_decode", 1, "hex_decode(String) -> Option<String>; base16, either case, white space between pairs skipped"},
	BHexEncode:    {BHexEncode, "hex_encode", 1, "hex_encode(String) -> String; lower-case base16 of the UTF-8 bytes"},
}

// LookupBuiltin returns the builtin with the given id.
func LookupBuiltin(id BuiltinID) (Builtin, bool) {
	if id >= numBuiltins {
		return Builtin{}, false
	}
	return builtins[id], true
}

// BuiltinByName finds a builtin by its name.
func BuiltinByName(name string) (Builtin, bool) {
	for _, b := range builtins {
		if b.Name == name {
			return b, true
		}
	}
	return Builtin{}, false
}

// Builtins returns the whole table in id order.
func Builtins() []Builtin {
	out := make([]Builtin, len(builtins))
	copy(out, builtins[:])
	return out
}

// NumBuiltins is the number of defined builtin ids.
const NumBuiltins = int(numBuiltins)
