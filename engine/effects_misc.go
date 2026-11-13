package engine

import (
	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/vm"
)

// doLog performs log(level, msg): a log event with the message capped at
// Config.LogCap bytes (spec 9.3).
func (r *runner) doLog(s vm.Suspended, e *effectState) error {
	if len(s.Args) != 2 {
		return r.failEffect(e, ActorEngine, EffectError{Code: "invalid_arguments", Message: "log takes level, message"}, bytecode.FailTypeMismatch)
	}
	msg, cut := truncateUTF8(s.Args[1].Str(), r.e.cfg.LogCap)
	return r.completeEffect(e, ActorEngine, vm.Unit(), ev(EvLog, ActorVM, LogPayload{
		Path: e.path, Level: enumName(r.mod, s.Args[0]), Message: msg, Truncated: cut,
	}))
}

// doAlert performs emit_alert(severity, msg): alert_emitted, which the
// outbox publishes to run.events (spec 9.5).
func (r *runner) doAlert(s vm.Suspended, e *effectState) error {
	if len(s.Args) != 2 {
		return r.failEffect(e, ActorEngine, EffectError{Code: "invalid_arguments", Message: "emit_alert takes severity, message"}, bytecode.FailTypeMismatch)
	}
	msg, _ := truncateUTF8(s.Args[1].Str(), r.e.cfg.LogCap)
	return r.completeEffect(e, ActorEngine, vm.Unit(), ev(EvAlertEmitted, ActorVM, AlertEmittedPayload{
		Path: e.path, Severity: enumName(r.mod, s.Args[0]), Message: msg,
	}))
}

// doIndexEffect performs embed(text) and similar(entity, k) through the
// optional Embedder and SimilarityIndex ports. Without a port the effect
// fails permanently. Results are recorded like tool results.
func (r *runner) doIndexEffect(s vm.Suspended, e *effectState) error {
	switch s.Kind {
	case bytecode.EffectEmbed:
		if r.e.d.Embedder == nil {
			return r.failEffect(e, ActorEngine, EffectError{Code: "embedder_unavailable", Message: "embed: no Embedder is configured"}, bytecode.FailEffectFailed)
		}
		if len(s.Args) < 1 {
			return r.failEffect(e, ActorEngine, EffectError{Code: "invalid_arguments", Message: "embed takes a text"}, bytecode.FailTypeMismatch)
		}
		vec, err := r.e.d.Embedder.Embed(r.ctx, r.rec.Tenant, s.Args[0].Str())
		if err != nil {
			return r.failEffect(e, ActorEngine, effectError(err), bytecode.FailEffectFailed)
		}
		vals := make([]vm.Value, len(vec))
		for i, f := range vec {
			vals[i] = vm.Float(f)
		}
		return r.completeEffect(e, ActorEngine, vm.List(vals...))
	default:
		if r.e.d.Similarity == nil {
			return r.failEffect(e, ActorEngine, EffectError{Code: "similarity_unavailable", Message: "similar: no SimilarityIndex is configured"}, bytecode.FailEffectFailed)
		}
		if len(s.Args) < 2 {
			return r.failEffect(e, ActorEngine, EffectError{Code: "invalid_arguments", Message: "similar takes an entity and k"}, bytecode.FailTypeMismatch)
		}
		ids, err := r.e.d.Similarity.Similar(r.ctx, r.rec.Tenant, s.Args[0].Str(), int(s.Args[1].AsInt()))
		if err != nil {
			return r.failEffect(e, ActorEngine, effectError(err), bytecode.FailEffectFailed)
		}
		vals := make([]vm.Value, len(ids))
		for i, id := range ids {
			vals[i] = vm.EntityRef(id)
		}
		return r.completeEffect(e, ActorEngine, vm.List(vals...))
	}
}
