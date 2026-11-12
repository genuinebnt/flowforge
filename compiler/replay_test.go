package compiler_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler"
	"github.com/genuinebnt/flowforge/vm"
)

// logEntry is one effect of log.json in conformance/vm/replay: the
// effect_scheduled fields (path, kind, args_hash) and the outcome
// (effect_completed with the canonical CBOR of the value, or
// effect_failed with a code).
type logEntry struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	ArgsHash string `json:"args_hash"`
	Value    string `json:"value_cbor,omitempty"`
	Fail     string `json:"fail,omitempty"`
	Message  string `json:"message,omitempty"`
}

// replayGolden is expected.json of a replay case.
type replayGolden struct {
	ModuleSHA256 string `json:"module_sha256"`
	golden
	Replayed int `json:"replayed"`
	Executed int `json:"executed"`
}

// TestVMConformance_Replay runs conformance/vm/replay (vm_004): the case's
// module (module.hex, which the compiler must still produce byte for
// byte) is run against the recorded event log with no live effects. An
// intact log reproduces the live run exactly; a tampered log (effects.json
// "tamper") fails with nondeterminism_detected.
func TestVMConformance_Replay(t *testing.T) {
	for _, c := range loadVMCases(t, "replay") {
		t.Run(c.name, func(t *testing.T) {
			res := compileCase(t, c)
			modPath := filepath.Join(c.dir, "module.hex")
			logPath := filepath.Join(c.dir, "log.json")
			if *update {
				writeFile(t, modPath, []byte(hex.EncodeToString(res.Encoded)+"\n"))
				writeReplayLog(t, res, c, logPath)
			}
			raw, err := os.ReadFile(modPath)
			if err != nil {
				t.Fatal(err)
			}
			modBytes, err := hex.DecodeString(string(bytes.TrimSpace(raw)))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(modBytes, res.Encoded) {
				t.Fatal("the compiler no longer produces module.hex for case.ff (regenerate with -update and review)")
			}
			m, err := bytecode.Decode(modBytes)
			if err != nil {
				t.Fatal(err)
			}
			prog, err := bytecode.Verify(m, res.Tools)
			if err != nil {
				t.Fatal(err)
			}
			entries := readReplayLog(t, prog, logPath)
			rp := vm.NewReplayer(entries, nil)
			d := &vmDriver{prog: prog, cfg: c.sc.vmConfig(), sc: &c.sc, h: rp.Handle, args: map[string]string{}}
			ro, err := d.run(caseInputs(t, res, c))
			if err != nil {
				t.Fatal(err)
			}
			g, err := toGolden(m, ro, d.args)
			if err != nil {
				t.Fatal(err)
			}
			got := replayGolden{ModuleSHA256: hex.EncodeToString(res.ModuleSHA256[:]), golden: g,
				Replayed: rp.Replayed, Executed: rp.Executed}
			if c.sc.Tamper == "" {
				// Replay equals live execution.
				live := runCaseVM(t, res, c, 0)
				if live.Failure != g.Failure || string(live.Value) != string(g.Value) || len(live.Trace) != len(g.Trace) {
					t.Fatalf("replay differs from the live run:\nlive %+v\nreplay %+v", live, g)
				}
			} else if g.Failure != bytecode.FailNondeterminismDetected.String() {
				t.Fatalf("tampered log replayed with %+v", g)
			}
			checkReplayGolden(t, filepath.Join(c.dir, "expected.json"), got)
		})
	}
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeReplayLog records the live run of c as log.json, corrupting the
// args_hash of the tampered path.
func writeReplayLog(t *testing.T, res *compiler.Result, c vmCase, path string) {
	t.Helper()
	d := &vmDriver{prog: res.Verified, cfg: c.sc.vmConfig(), sc: &c.sc, h: c.sc.handler(res.Module)}
	ro, err := d.run(caseInputs(t, res, c))
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]logEntry, 0, len(ro.trace))
	tampered := false
	for _, e := range ro.trace {
		le := logEntry{Path: e.Path, Kind: e.Kind.String(), ArgsHash: hex.EncodeToString(e.ArgsHash[:])}
		if e.Path == c.sc.Tamper {
			h := e.ArgsHash
			h[0] ^= 0xff
			le.ArgsHash = hex.EncodeToString(h[:])
			tampered = true
		}
		if e.Result.Failed {
			le.Fail, le.Message = e.Result.Code.String(), e.Result.Message
		} else {
			b, err := vm.EncodeValue(e.Result.Value)
			if err != nil {
				t.Fatal(err)
			}
			le.Value = hex.EncodeToString(b)
		}
		entries = append(entries, le)
	}
	if c.sc.Tamper != "" && !tampered {
		t.Fatalf("tamper path %s not in the trace", c.sc.Tamper)
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, append(b, '\n'))
}

// readReplayLog decodes log.json into the trace the Replayer consumes.
func readReplayLog(t *testing.T, prog *bytecode.Verified, path string) []vm.TraceEntry {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entries []logEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	out := make([]vm.TraceEntry, len(entries))
	for i, le := range entries {
		kind, ok := bytecode.ParseEffectKind(le.Kind)
		h, err := hex.DecodeString(le.ArgsHash)
		if !ok || err != nil || len(h) != 32 {
			t.Fatalf("entry %d: bad kind or hash", i)
		}
		e := vm.TraceEntry{Path: le.Path, Kind: kind}
		copy(e.ArgsHash[:], h)
		if le.Fail != "" {
			code, _ := bytecode.ParseFailCode(le.Fail)
			e.Result = vm.FailResult(code, le.Message)
		} else {
			b, err := hex.DecodeString(le.Value)
			if err != nil {
				t.Fatal(err)
			}
			v, err := vm.DecodeValue(prog, b)
			if err != nil {
				t.Fatalf("entry %d: %v", i, err)
			}
			e.Result = vm.OkResult(v)
		}
		out[i] = e
	}
	return out
}

func checkReplayGolden(t *testing.T, path string, got replayGolden) {
	t.Helper()
	b, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	if *update {
		writeFile(t, path, b)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(b, &g)
	if !reflect.DeepEqual(w, g) {
		t.Fatalf("golden mismatch for %s\n--- want\n%s\n--- got\n%s", path, want, b)
	}
}
