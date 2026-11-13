package htmlparse

import (
	"fmt"
	"os"
	"testing"
)

func TestExploreOracle(t *testing.T) {
	var out []string
	for _, c := range loadDatSuite(t) {
		if c.ScriptOn {
			continue
		}
		var o string
		var ok bool
		if c.HasFrag {
			o, ok = oracleParseFragment([]byte(c.Data), parseContext(c.Fragment))
		} else {
			o, ok = oracleParse([]byte(c.Data))
		}
		if !ok || o == c.Document {
			continue
		}
		out = append(out, fmt.Sprintf("=== %s ctx=%q\n%s\n--- oracle\n%s\n--- want\n%s\n", c.ID, c.Fragment, c.Data, o, c.Document))
	}
	os.WriteFile(os.Getenv("OUT"), []byte(fmt.Sprint(len(out))+"\n"+fmt.Sprint(out)), 0o644)
}
