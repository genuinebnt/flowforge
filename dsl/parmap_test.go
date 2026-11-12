package dsl

import "testing"

// TestDSLParMapOnError: on_error must be a literal; "collect" types the
// result as Seq<Result<B, TaskError>> (spec 8.6).
func TestDSLParMapOnError(t *testing.T) {
	ok := `tool probe(n: Int) -> Int ! active
workflow w(xs: List<Int>) -> List<Result<Int, TaskError>> {
  xs |> par_map(probe, limit = 2, on_error = "collect")
}`
	f, d := Parse("p.ff", []byte(ok))
	if _, cd := Check(f, Options{}); HasErrors(append(d, cd...)) {
		t.Fatalf("collect: %v %v", d, cd)
	}
	bad := `tool probe(n: Int) -> Int ! active
workflow w(xs: List<Int>, mode: String) -> List<Int> {
  xs |> par_map(probe, limit = 2, on_error = mode)
}`
	f, _ = Parse("p.ff", []byte(bad))
	_, cd := Check(f, Options{})
	if len(cd) == 0 || cd[0].Code != CodeBadNamedArg {
		t.Fatalf("non-literal on_error: %v", cd)
	}
}
