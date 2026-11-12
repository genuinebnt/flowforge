package engine

// Code below is adapted from engine/enginetest (MemRepo, MemTools), which
// non-test code may not import; it is the dev stack's in-memory engine.Repo
// and engine.ToolRegistry.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
)

// MemRepo is the in-memory engine.Repo. The zero tenant is the system
// scope (it sees every tenant's rows); any other tenant sees only its own.
// It is safe for concurrent use.
type MemRepo struct {
	mu        sync.Mutex
	workflows map[core.UUID]map[string]core.UUID // tenant -> name -> workflow id
	versions  map[core.UUID][]engine.WorkflowVersion
	byID      map[core.UUID]engine.WorkflowVersion
	runs      map[core.UUID]engine.RunRecord
	keys      map[string]core.UUID
	requests  map[core.UUID][]memRequest
	nextReq   int64
	approvals map[core.UUID]engine.Approval
	apprPath  map[string]core.UUID
	timers    map[core.UUID]engine.Timer
	effects   map[core.UUID]map[string]engine.EffectRecord
	ids       core.IDGen
}

type memRequest struct {
	req     engine.RunRequest
	applied bool
}

var _ engine.Repo = (*MemRepo)(nil)

// NewMemRepo returns an empty repository. ids generates workflow ids.
func NewMemRepo(ids core.IDGen) *MemRepo {
	return &MemRepo{
		workflows: map[core.UUID]map[string]core.UUID{}, versions: map[core.UUID][]engine.WorkflowVersion{},
		byID: map[core.UUID]engine.WorkflowVersion{}, runs: map[core.UUID]engine.RunRecord{},
		keys: map[string]core.UUID{}, requests: map[core.UUID][]memRequest{},
		approvals: map[core.UUID]engine.Approval{}, apprPath: map[string]core.UUID{},
		timers: map[core.UUID]engine.Timer{}, effects: map[core.UUID]map[string]engine.EffectRecord{}, ids: ids,
	}
}

func visible(tenant, owner core.UUID) bool { return tenant.IsZero() || tenant == owner }

func cloneJSON(b json.RawMessage) json.RawMessage {
	if b == nil {
		return nil
	}
	return bytes.Clone(b)
}

// CreateWorkflowVersion implements engine.Repo.
func (m *MemRepo) CreateWorkflowVersion(ctx context.Context, v engine.WorkflowVersion) (engine.WorkflowVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workflows[v.Tenant] == nil {
		m.workflows[v.Tenant] = map[string]core.UUID{}
	}
	wf, ok := m.workflows[v.Tenant][v.Name]
	if !ok {
		wf = m.ids.New()
		m.workflows[v.Tenant][v.Name] = wf
	}
	v.WorkflowID = wf
	v.Version = int32(len(m.versions[wf]) + 1)
	v.Module = bytes.Clone(v.Module)
	m.versions[wf] = append(m.versions[wf], v)
	m.byID[v.ID] = v
	return v, nil
}

// GetWorkflowVersion implements engine.Repo.
func (m *MemRepo) GetWorkflowVersion(ctx context.Context, tenant core.UUID, name string, n int32) (engine.WorkflowVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	wf, ok := m.workflows[tenant][name]
	if !ok {
		return engine.WorkflowVersion{}, engine.ErrNotFound
	}
	vs := m.versions[wf]
	if n > 0 {
		if int(n) > len(vs) {
			return engine.WorkflowVersion{}, engine.ErrNotFound
		}
		return vs[n-1], nil
	}
	for i := len(vs) - 1; i >= 0; i-- {
		if vs[i].Status == engine.VersionPublished {
			return vs[i], nil
		}
	}
	return engine.WorkflowVersion{}, engine.ErrNotFound
}

// GetWorkflowVersionByID implements engine.Repo.
func (m *MemRepo) GetWorkflowVersionByID(ctx context.Context, tenant, id core.UUID) (engine.WorkflowVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.byID[id]
	if !ok || !visible(tenant, v.Tenant) {
		return engine.WorkflowVersion{}, engine.ErrNotFound
	}
	return m.versions[v.WorkflowID][v.Version-1], nil
}

// ListWorkflows implements engine.Repo.
func (m *MemRepo) ListWorkflows(ctx context.Context, tenant core.UUID, afterName string, limit int) ([]engine.WorkflowVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var names []string
	for n := range m.workflows[tenant] {
		if n > afterName {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	var out []engine.WorkflowVersion
	for _, n := range names {
		if len(out) == limit {
			break
		}
		vs := m.versions[m.workflows[tenant][n]]
		out = append(out, vs[len(vs)-1])
	}
	return out, nil
}

// SetWorkflowVersionStatus implements engine.Repo.
func (m *MemRepo) SetWorkflowVersionStatus(ctx context.Context, tenant core.UUID, name string, n int32, status string) (engine.WorkflowVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	wf, ok := m.workflows[tenant][name]
	if !ok || n < 1 || int(n) > len(m.versions[wf]) {
		return engine.WorkflowVersion{}, engine.ErrNotFound
	}
	v := &m.versions[wf][n-1]
	v.Status = status
	m.byID[v.ID] = *v
	return *v, nil
}

func keyOf(tenant core.UUID, key string) string { return tenant.String() + "/" + key }

// InsertRun implements engine.Repo.
func (m *MemRepo) InsertRun(ctx context.Context, r engine.RunRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := keyOf(r.Tenant, r.IdempotencyKey)
	if _, ok := m.runs[r.ID]; ok {
		return engine.ErrConflict
	}
	if _, ok := m.keys[k]; ok {
		return engine.ErrConflict
	}
	m.runs[r.ID] = r
	m.keys[k] = r.ID
	return nil
}

// GetRun implements engine.Repo.
func (m *MemRepo) GetRun(ctx context.Context, tenant, id core.UUID) (engine.RunRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok || !visible(tenant, r.Tenant) {
		return engine.RunRecord{}, engine.ErrNotFound
	}
	return r, nil
}

// FindRunByIdempotencyKey implements engine.Repo.
func (m *MemRepo) FindRunByIdempotencyKey(ctx context.Context, tenant core.UUID, key string) (engine.RunRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.keys[keyOf(tenant, key)]
	if !ok {
		return engine.RunRecord{}, engine.ErrNotFound
	}
	return m.runs[id], nil
}

func runBefore(a, b engine.RunRecord) int {
	if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
		return c
	}
	return bytes.Compare(b.ID[:], a.ID[:])
}

// ListRuns implements engine.Repo.
func (m *MemRepo) ListRuns(ctx context.Context, tenant core.UUID, f engine.RunFilter, after engine.RunCursor, limit int) ([]engine.RunRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []engine.RunRecord
	cur := engine.RunRecord{CreatedAt: after.CreatedAt, ID: after.ID}
	for _, r := range m.runs {
		switch {
		case !visible(tenant, r.Tenant),
			!f.ProgramID.IsZero() && r.ProgramID != f.ProgramID,
			f.Status != "" && r.Status != f.Status,
			f.WorkflowName != "" && r.WorkflowName != f.WorkflowName,
			!f.CreatedAfter.IsZero() && !r.CreatedAt.After(f.CreatedAfter),
			!after.ID.IsZero() && runBefore(cur, r) >= 0:
			continue
		}
		out = append(out, r)
	}
	slices.SortFunc(out, runBefore)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListActiveRuns implements engine.Repo.
func (m *MemRepo) ListActiveRuns(ctx context.Context, tenant, program core.UUID) ([]engine.RunRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []engine.RunRecord
	for _, r := range m.runs {
		if visible(tenant, r.Tenant) && (program.IsZero() || r.ProgramID == program) && !engine.Terminal(r.Status) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b engine.RunRecord) int { return bytes.Compare(a.ID[:], b.ID[:]) })
	return out, nil
}

// UpdateRun implements engine.Repo.
func (m *MemRepo) UpdateRun(ctx context.Context, r engine.RunRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.runs[r.ID]
	if !ok {
		return engine.ErrNotFound
	}
	cur.Status, cur.CompensationOutcome = r.Status, r.CompensationOutcome
	cur.Output, cur.FailureCode, cur.FailureMessage = cloneJSON(r.Output), r.FailureCode, r.FailureMessage
	cur.ScopeVersion, cur.StartedAt, cur.FinishedAt = r.ScopeVersion, r.StartedAt, r.FinishedAt
	cur.LastSeq, cur.PublishedSeq = r.LastSeq, r.PublishedSeq
	m.runs[r.ID] = cur
	return nil
}

// AddRunRequest implements engine.Repo.
func (m *MemRepo) AddRunRequest(ctx context.Context, q engine.RunRequest) (engine.RunRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[q.RunID]; !ok {
		return engine.RunRequest{}, engine.ErrNotFound
	}
	m.nextReq++
	q.ID = m.nextReq
	m.requests[q.RunID] = append(m.requests[q.RunID], memRequest{req: q})
	return q, nil
}

// PendingRunRequests implements engine.Repo.
func (m *MemRepo) PendingRunRequests(ctx context.Context, run core.UUID) ([]engine.RunRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []engine.RunRequest
	for _, q := range m.requests[run] {
		if !q.applied {
			out = append(out, q.req)
		}
	}
	return out, nil
}

// MarkRunRequestsApplied implements engine.Repo.
func (m *MemRepo) MarkRunRequestsApplied(ctx context.Context, run core.UUID, ids []int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	qs := m.requests[run]
	for i := range qs {
		if slices.Contains(ids, qs[i].req.ID) {
			qs[i].applied = true
		}
	}
	return nil
}

// UpsertApproval implements engine.Repo.
func (m *MemRepo) UpsertApproval(ctx context.Context, a engine.Approval) (engine.Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := a.RunID.String() + "/" + a.EffectPath
	if id, ok := m.apprPath[k]; ok {
		return m.approvals[id], nil
	}
	if _, ok := m.approvals[a.ID]; ok {
		return engine.Approval{}, engine.ErrConflict
	}
	a.Proposed = cloneJSON(a.Proposed)
	m.approvals[a.ID] = a
	m.apprPath[k] = a.ID
	return a, nil
}

// GetApproval implements engine.Repo.
func (m *MemRepo) GetApproval(ctx context.Context, tenant, id core.UUID) (engine.Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok || !visible(tenant, a.Tenant) {
		return engine.Approval{}, engine.ErrNotFound
	}
	return a, nil
}

// ListApprovals implements engine.Repo.
func (m *MemRepo) ListApprovals(ctx context.Context, tenant core.UUID, f engine.ApprovalFilter, after core.UUID, limit int) ([]engine.Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []engine.Approval
	for _, a := range m.approvals {
		if !visible(tenant, a.Tenant) || (f.Status != "" && a.Status != f.Status) ||
			(f.Role != "" && a.Role != f.Role) || (!f.RunID.IsZero() && a.RunID != f.RunID) {
			continue
		}
		out = append(out, a)
	}
	order := func(a, b engine.Approval) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return bytes.Compare(a.ID[:], b.ID[:])
	}
	slices.SortFunc(out, order)
	if !after.IsZero() {
		if i := slices.IndexFunc(out, func(a engine.Approval) bool { return a.ID == after }); i >= 0 {
			out = out[i+1:]
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// DecideApproval implements engine.Repo.
func (m *MemRepo) DecideApproval(ctx context.Context, tenant, id core.UUID, status, actor, comment string, at time.Time) (engine.Approval, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok || !visible(tenant, a.Tenant) {
		return engine.Approval{}, false, engine.ErrNotFound
	}
	if a.Status != engine.ApprovalPending {
		return a, false, nil
	}
	a.Status, a.DecidedBy, a.Comment, a.DecidedAt = status, actor, comment, at
	m.approvals[id] = a
	return a, true, nil
}

// UpsertTimer implements engine.Repo.
func (m *MemRepo) UpsertTimer(ctx context.Context, t engine.Timer) (engine.Timer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.timers[t.ID]; ok {
		return cur, nil
	}
	m.timers[t.ID] = t
	return t, nil
}

// FireTimer implements engine.Repo.
func (m *MemRepo) FireTimer(ctx context.Context, id core.UUID, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.timers[id]
	if !ok {
		return engine.ErrNotFound
	}
	if t.Fired.IsZero() {
		t.Fired = at
		m.timers[id] = t
	}
	return nil
}

// UpsertEffect implements engine.Repo.
func (m *MemRepo) UpsertEffect(ctx context.Context, e engine.EffectRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.effects[e.RunID] == nil {
		m.effects[e.RunID] = map[string]engine.EffectRecord{}
	}
	m.effects[e.RunID][e.Path] = e
	return nil
}

// ListEffects implements engine.Repo.
func (m *MemRepo) ListEffects(ctx context.Context, run core.UUID) ([]engine.EffectRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]engine.EffectRecord, 0, len(m.effects[run]))
	for _, e := range m.effects[run] {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b engine.EffectRecord) int { return cmp.Compare(a.Seq, b.Seq) })
	return out, nil
}

// Timers returns every timer (test helper).
func (m *MemRepo) Timers() []engine.Timer {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]engine.Timer, 0, len(m.timers))
	for _, t := range m.timers {
		out = append(out, t)
	}
	return out
}
