package jsparse

import (
	"os"
	"testing"
)

func TestDbg(t *testing.T) {
	src := os.Getenv("DBG")
	prog, err := Parse([]byte(src), Options{})
	if err != nil {
		t.Fatal(err)
	}
	out := Print(prog, PrintOptions{Minify: os.Getenv("MIN") != ""})
	t.Logf("printed: %s", out)
	again, err := Parse(out, Options{})
	t.Logf("err=%v\n%s\n%s", err, prog.JSON(JSONOptions{}), again.JSON(JSONOptions{}))
}
