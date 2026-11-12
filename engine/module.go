package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/vm"
)

// loadedModule is a decoded, verified module with the specs of the tools
// it pins.
type loadedModule struct {
	prog  *bytecode.Verified
	tools map[string]ToolSpec // by name
}

// loadModule decodes an encoded module, resolves every TOOLS entry to the
// registered spec with the pinned hash, and runs the verifier (spec 8.8).
func (e *Engine) loadModule(ctx context.Context, encoded []byte) (*loadedModule, error) {
	m, err := bytecode.Decode(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode module: %w", err)
	}
	tools := make(map[string]ToolSpec, len(m.Tools))
	resolver := bytecode.ToolMap{}
	for i, t := range m.Tools {
		name := m.ToolName(i)
		spec, err := e.d.Tools.ResolveTool(ctx, name, t.SpecSHA256)
		if err != nil {
			return nil, fmt.Errorf("tool %q (spec %x): %w", name, t.SpecSHA256[:6], err)
		}
		tools[name] = spec
		resolver[name] = bytecode.ToolInfo{SpecSHA256: t.SpecSHA256, Effects: effectMask(spec.EffectClass)}
	}
	prog, err := bytecode.Verify(m, resolver)
	if err != nil {
		return nil, fmt.Errorf("verify module: %w", err)
	}
	return &loadedModule{prog: prog, tools: tools}, nil
}

// agentEffects reports the agent effect kinds (agent_call, execute_plan)
// that appear in any function of m. Registration rejects such modules
// (docs/DECISIONS.md D1); the scan covers unreachable code too, so the
// rejection does not depend on the verifier's reachability analysis.
func agentEffects(m *bytecode.Module) []bytecode.EffectKind {
	seen := map[bytecode.EffectKind]bool{}
	var out []bytecode.EffectKind
	for _, f := range m.Funcs {
		code := f.Code
		for pc := 0; pc < len(code); {
			op := bytecode.Opcode(code[pc])
			size := op.Size()
			if size == 0 || pc+size > len(code) {
				break
			}
			if op == bytecode.OpEffect {
				k := bytecode.EffectKind(code[pc+1])
				if (k == bytecode.EffectAgentCall || k == bytecode.EffectExecutePlan) && !seen[k] {
					seen[k] = true
					out = append(out, k)
				}
			}
			pc += size
		}
	}
	return out
}

// workflowAttributes merges the workflow annotations of the module's
// ANNOTS section (bytecode.Annotations) with those the compiler reported
// next to the module; the stricter value wins.
func workflowAttributes(m *bytecode.Module, reported WorkflowAttributes) WorkflowAttributes {
	a := reported
	if an := m.Annotations; an != nil {
		if t := time.Duration(an.Timeout); t > 0 && (a.Timeout == 0 || t < a.Timeout) {
			a.Timeout = t
		}
		if n := int(an.MaxEffects); n > 0 && (a.MaxEffects == 0 || n < a.MaxEffects) {
			a.MaxEffects = n
		}
		a.OnUnknownRetry = a.OnUnknownRetry || an.OnUnknownRetry
	}
	return a
}

// siteAnnotation returns the per-effect policy of the EFFECT instruction a
// suspension stopped at.
func siteAnnotation(m *bytecode.Module, site vm.Span) (bytecode.SiteAnnotation, bool) {
	if m.Annotations == nil || site.Func < 0 || site.CodeOffset < 0 {
		return bytecode.SiteAnnotation{}, false
	}
	return m.Annotations.Site(uint16(site.Func), uint32(site.CodeOffset))
}
