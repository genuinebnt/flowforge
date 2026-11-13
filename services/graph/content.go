package graph

import (
	"context"
	"encoding/json"
	"errors"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/genuinebnt/flowforge/contentstore"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/services/internal/convert"
)

// ContentConsumer is the bus consumer name of the graph on content.changes.
const ContentConsumer = "graph"

// HandleContentEvent is the bus handler of content.changes (V2 §3.4,
// §6): it ingests the event's oam observation (content.Observe) and, for
// a change, records content_changed on the observation's source entity.
// Both are idempotent (observation id; ContentChangeUUID), so redelivery
// is harmless. Malformed events are logged and dropped; store errors are
// returned for redelivery.
func (s *Server) HandleContentEvent(ctx context.Context, env *pb.EventEnvelope) error {
	tenant, err := convert.EnvelopeTenant(env)
	if err != nil {
		s.o.Logger.WarnContext(ctx, "graph.content_event_bad", "event_id", env.GetEventId(), "error", err.Error())
		return nil
	}
	raw, err := protojson.Marshal(env.GetGeneric())
	if err != nil {
		return nil
	}
	// Lenient decode: a newer producer may add fields.
	var p contentstore.EventPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		s.o.Logger.WarnContext(ctx, "graph.content_event_bad", "event_id", env.GetEventId(), "error", err.Error())
		return nil
	}
	obs, err := oam.DecodeObservation([]byte(p.Observation), oam.Strict)
	if err != nil {
		s.o.Logger.WarnContext(ctx, "graph.content_event_bad", "event_id", env.GetEventId(), "error", err.Error())
		return nil
	}
	tid := core.PublicID(core.PrefixTenant, tenant)
	program := env.GetProgramId()
	res, err := s.o.Store.IngestObservation(ctx, graphstore.IngestRequest{TenantID: tid, ProgramID: program, RunID: p.RunID,
		EffectPath: p.EffectPath, ToolName: obs.Tool.Name, ToolVersion: obs.Tool.Version, Observation: obs})
	if err != nil {
		if errors.Is(err, oam.ErrInvalidObservation) {
			s.o.Logger.WarnContext(ctx, "graph.content_event_rejected", "event_id", env.GetEventId(), "error", err.Error())
			return nil
		}
		return err
	}
	s.publish(ctx, res.Changes)
	if p.ContentChange == nil || obs.Authority == nil {
		return nil
	}
	entity := res.RefToEntityID[obs.Authority.Source]
	if entity == "" {
		// A redelivered observation reports no refs; find the source by key.
		if entity, err = s.sourceEntity(ctx, tid, program, obs); err != nil || entity == "" {
			return err
		}
	}
	ev, err := s.o.Store.RecordContentChange(ctx, graphstore.Caller{TenantID: tid},
		graphstore.ContentChange{EntityID: entity, RunID: p.RunID, At: obs.ObservedAt, Patch: *p.ContentChange})
	if err != nil {
		if errors.Is(err, oam.ErrInvalidObservation) {
			s.o.Logger.WarnContext(ctx, "graph.content_change_rejected", "event_id", env.GetEventId(), "error", err.Error())
			return nil
		}
		return err
	}
	s.publish(ctx, []graphstore.ChangeEvent{ev})
	return nil
}

// sourceEntity finds the entity of the observation's authority source.
func (s *Server) sourceEntity(ctx context.Context, tenant, program string, obs oam.Observation) (string, error) {
	for _, e := range obs.Entities {
		if e.Ref != obs.Authority.Source {
			continue
		}
		key, err := e.Asset.Key()
		if err != nil {
			return "", nil
		}
		r, err := s.o.Store.FindEntities(ctx, graphstore.Caller{TenantID: tenant}, graphstore.FindEntitiesRequest{ProgramID: program,
			AssetTypes: []string{e.Asset.AssetType()}, KeyPrefix: key, Page: graphstore.PageRequest{PageSize: 10}})
		if err != nil {
			return "", err
		}
		for _, x := range r.Entities {
			if x.Key == key {
				return x.ID, nil
			}
		}
	}
	return "", nil
}
