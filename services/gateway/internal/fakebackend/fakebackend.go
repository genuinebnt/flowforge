// Package fakebackend runs in-memory fakes of every internal Flowforge gRPC
// service behind real gRPC servers on bufconn, for gateway tests. Each call
// must carry a valid internal token (aud flowforge-internal) whose tenant
// equals x-tenant-id; data is partitioned by that tenant, so tests prove
// the gateway forwards identity from the token and nowhere else.
package fakebackend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	graphv1 "github.com/genuinebnt/flowforge/gen/flowforge/graph/v1"
	simv1 "github.com/genuinebnt/flowforge/gen/flowforge/similarity/v1"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/gateway"
)

// Call records the metadata of one received call.
type Call struct {
	Method, Tenant, Actor, Role, RequestID, Traceparent, IdempotencyKey string
}

// Backend holds the fake state of every service.
type Backend struct {
	clock core.Clock
	mu    sync.Mutex
	cond  *sync.Cond
	ids   *core.SplitMix64
	calls []Call

	programs  map[string]*program
	workflows map[string]map[string][]*pb.WorkflowVersion // tenant -> name -> versions
	runs      map[string]*run
	runIdem   map[string]string // tenant|key -> run id
	approvals map[string]*approval
	entities  map[string]*entity
	edgesByP  map[string][]*pb.Edge
	changes   map[string][]*pb.ChangeEvent // program -> changes
	rules     map[string]*rule
	channels  map[string]*channel
	seq       int64
}

type program struct {
	tenant   string
	p        *pb.Program
	versions []*pb.ScopeVersion
	kill     bool
}

type run struct {
	tenant string
	r      *pb.Run
	events []*pb.RunEvent
	hash   string
}

type approval struct {
	tenant string
	a      *pb.Approval
}

type entity struct {
	tenant string
	e      *pb.Entity
}

type rule struct {
	tenant string
	r      *pb.AlertRule
}

type channel struct {
	tenant string
	c      *pb.Channel
}

type caller struct {
	tenant, actor, sub string
	role               auth.Role
	idem               string
}

type ctxKey struct{}

func from(ctx context.Context) caller { c, _ := ctx.Value(ctxKey{}).(caller); return c }

// Start serves the fakes on bufconn and returns the backend plus gateway
// clients dialled with gateway.DialOptions(minter).
func Start(t testing.TB, clock core.Clock, keys auth.KeyResolver, issuer string, minter gateway.MinterSource) (*Backend, gateway.Backends) {
	t.Helper()
	b := &Backend{
		clock: clock, ids: core.NewSplitMix64(77),
		programs: map[string]*program{}, workflows: map[string]map[string][]*pb.WorkflowVersion{}, runs: map[string]*run{},
		runIdem: map[string]string{}, approvals: map[string]*approval{}, entities: map[string]*entity{},
		edgesByP: map[string][]*pb.Edge{}, changes: map[string][]*pb.ChangeEvent{}, rules: map[string]*rule{}, channels: map[string]*channel{},
	}
	b.cond = sync.NewCond(&b.mu)
	v, err := auth.NewVerifier(auth.InternalVerifierConfig(keys, clock, issuer))
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			ctx, err := b.authenticate(ctx, v, info.FullMethod)
			if err != nil {
				return nil, err
			}
			return h(ctx, req)
		}),
		grpc.ChainStreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			ctx, err := b.authenticate(ss.Context(), v, info.FullMethod)
			if err != nil {
				return err
			}
			return h(srv, wrapped{ss, ctx})
		}),
	)
	pb.RegisterScopeServiceServer(srv, scopeSrv{b: b})
	pb.RegisterEngineServiceServer(srv, engineSrv{b: b})
	pb.RegisterGraphServiceServer(srv, graphSrv{b: b})
	pb.RegisterNotifierServiceServer(srv, notifierSrv{b: b})
	pb.RegisterToolRunnerServiceServer(srv, toolSrv{})
	graphv1.RegisterGraphAnalyticsServer(srv, analyticsSrv{b: b})
	simv1.RegisterSimilarityServer(srv, simSrv{b: b})
	go func() { _ = srv.Serve(lis) }()
	opts := append([]grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, gateway.DialOptions(minter)...)
	cc, err := grpc.NewClient("passthrough:///bufnet", opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		b.mu.Lock()
		b.cond.Broadcast()
		b.mu.Unlock()
		_ = cc.Close()
		srv.Stop()
	})
	return b, gateway.BackendsFromConns(cc, cc, cc, cc, cc, cc)
}

type wrapped struct {
	grpc.ServerStream
	ctx context.Context
}

func (w wrapped) Context() context.Context { return w.ctx }

func (b *Backend) authenticate(ctx context.Context, v *auth.Verifier, method string) (context.Context, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	get := func(k string) string {
		if vs := md.Get(k); len(vs) > 0 {
			return vs[0]
		}
		return ""
	}
	tok, ok := strings.CutPrefix(get("authorization"), "Bearer ")
	if !ok {
		return nil, fail(codes.Unauthenticated, core.ReasonTokenMissing, "no internal token")
	}
	claims, err := v.Verify(ctx, tok)
	if err != nil {
		return nil, fail(codes.Unauthenticated, core.ReasonTokenInvalid, "internal token: "+auth.RejectionReason(err))
	}
	if get("x-tenant-id") != claims.TenantID || get("x-actor") == "" || get("x-request-id") == "" || get("traceparent") == "" {
		return nil, fail(codes.PermissionDenied, core.ReasonForbiddenRole, "metadata disagrees with the internal token")
	}
	c := caller{tenant: claims.TenantID, actor: get("x-actor"), sub: claims.Subject, role: claims.Role, idem: get("idempotency-key")}
	b.mu.Lock()
	b.calls = append(b.calls, Call{Method: method, Tenant: c.tenant, Actor: c.actor, Role: string(c.role), RequestID: get("x-request-id"),
		Traceparent: get("traceparent"), IdempotencyKey: c.idem})
	b.mu.Unlock()
	return context.WithValue(ctx, ctxKey{}, c), nil
}

// Calls returns the calls received so far.
func (b *Backend) Calls() []Call {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Call(nil), b.calls...)
}

func fail(code codes.Code, reason, msg string, meta ...string) error {
	st := status.New(code, msg)
	info := &errdetails.ErrorInfo{Reason: reason, Domain: "fake.flowforge", Metadata: map[string]string{}}
	for i := 0; i+1 < len(meta); i += 2 {
		info.Metadata[meta[i]] = meta[i+1]
	}
	details := []protoadapt.MessageV1{protoadapt.MessageV1Of(info)}
	if code == codes.ResourceExhausted {
		details = append(details, protoadapt.MessageV1Of(&errdetails.RetryInfo{RetryDelay: durationpb.New(2 * time.Second)}))
	}
	st2, err := st.WithDetails(details...)
	if err != nil {
		return st.Err()
	}
	return st2.Err()
}

func notFound() error { return fail(codes.NotFound, core.ReasonNotFound, "not found") }

func (b *Backend) id(p core.Prefix) string {
	return core.PublicID(p, core.NewUUIDv7(b.clock, b.ids))
}

func (b *Backend) now() *timestamppb.Timestamp { return timestamppb.New(b.clock.Now()) }

// page applies an offset page token ("o<N>").
func page[T any](items []T, p *pb.PageRequest) ([]T, *pb.PageResponse, error) {
	size := int(p.GetPageSize())
	if size <= 0 {
		size = 50
	}
	off := 0
	if tok := p.GetPageToken(); tok != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(tok, "o"))
		if err != nil || !strings.HasPrefix(tok, "o") || n < 0 {
			return nil, nil, fail(codes.InvalidArgument, core.ReasonValidationFailed, "invalid page_token")
		}
		off = n
	}
	if off > len(items) {
		off = len(items)
	}
	end := min(off+size, len(items))
	next := ""
	if end < len(items) {
		next = "o" + strconv.Itoa(end)
	}
	return items[off:end], &pb.PageResponse{NextPageToken: next}, nil
}

func (b *Backend) programOf(ctx context.Context, id string) (*program, error) {
	p, ok := b.programs[id]
	if !ok || p.tenant != from(ctx).tenant {
		return nil, notFound()
	}
	return p, nil
}

// ---- scope -------------------------------------------------------------

type scopeSrv struct {
	pb.UnimplementedScopeServiceServer
	b *Backend
}

func (s scopeSrv) CreateProgram(ctx context.Context, r *pb.CreateProgramRequest) (*pb.Program, error) {
	b := s.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.GetName() == "" {
		return nil, fail(codes.InvalidArgument, core.ReasonValidationFailed, "name required")
	}
	p := &pb.Program{Id: b.id(core.PrefixProgram), Name: r.GetName(), Platform: r.GetPlatform(), Status: "active", CurrentVersion: 1, CreatedAt: b.now()}
	sv := &pb.ScopeVersion{ProgramId: p.Id, Version: 1, Document: r.GetInitialScope(), EffectiveFrom: b.now(), CreatedBy: from(ctx).sub, CreatedAt: b.now()}
	b.programs[p.Id] = &program{tenant: from(ctx).tenant, p: p, versions: []*pb.ScopeVersion{sv}}
	b.seedGraph(from(ctx).tenant, p.Id)
	return p, nil
}

func (s scopeSrv) GetProgram(ctx context.Context, r *pb.GetProgramRequest) (*pb.Program, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	p, err := s.b.programOf(ctx, r.GetProgramId())
	if err != nil {
		return nil, err
	}
	return p.p, nil
}

func (s scopeSrv) ListPrograms(ctx context.Context, r *pb.ListProgramsRequest) (*pb.ListProgramsResponse, error) {
	b := s.b
	b.mu.Lock()
	defer b.mu.Unlock()
	var all []*pb.Program
	for _, p := range b.programs {
		if p.tenant == from(ctx).tenant {
			all = append(all, p.p)
		}
	}
	sortBy(all, func(p *pb.Program) string { return p.Id })
	items, pg, err := page(all, r.GetPage())
	if err != nil {
		return nil, err
	}
	return &pb.ListProgramsResponse{Programs: items, Page: pg}, nil
}

func (s scopeSrv) PutScope(ctx context.Context, r *pb.PutScopeRequest) (*pb.ScopeVersion, error) {
	b := s.b
	b.mu.Lock()
	defer b.mu.Unlock()
	p, err := b.programOf(ctx, r.GetProgramId())
	if err != nil {
		return nil, err
	}
	if r.GetBaseVersion() != p.p.CurrentVersion {
		return nil, fail(codes.Aborted, core.ReasonVersionConflict, fmt.Sprintf("base_version %d is not the current version %d", r.GetBaseVersion(), p.p.CurrentVersion),
			"current_version", strconv.Itoa(int(p.p.CurrentVersion)))
	}
	p.p.CurrentVersion++
	eff := r.GetEffectiveFrom()
	if eff == nil {
		eff = b.now()
	}
	sv := &pb.ScopeVersion{ProgramId: p.p.Id, Version: p.p.CurrentVersion, Document: r.GetDocument(), EffectiveFrom: eff, CreatedBy: from(ctx).sub, CreatedAt: b.now()}
	p.versions = append(p.versions, sv)
	return sv, nil
}

func (s scopeSrv) GetScope(ctx context.Context, r *pb.GetScopeRequest) (*pb.ScopeVersion, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	p, err := s.b.programOf(ctx, r.GetProgramId())
	if err != nil {
		return nil, err
	}
	v := r.GetVersion()
	if v == 0 {
		v = p.p.CurrentVersion
	}
	if v < 1 || int(v) > len(p.versions) {
		return nil, notFound()
	}
	return p.versions[v-1], nil
}

func (s scopeSrv) ListScopeVersions(ctx context.Context, r *pb.ListScopeVersionsRequest) (*pb.ListScopeVersionsResponse, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	p, err := s.b.programOf(ctx, r.GetProgramId())
	if err != nil {
		return nil, err
	}
	items, pg, err := page(p.versions, r.GetPage())
	if err != nil {
		return nil, err
	}
	return &pb.ListScopeVersionsResponse{Versions: items, Page: pg}, nil
}

func (s scopeSrv) Check(ctx context.Context, r *pb.CheckRequest) (*pb.Decision, error) {
	b := s.b
	b.mu.Lock()
	defer b.mu.Unlock()
	p, err := b.programOf(ctx, r.GetProgramId())
	if err != nil {
		return nil, err
	}
	host := strings.ToLower(r.GetTarget())
	norm := host
	if i := strings.Index(host, "://"); i >= 0 {
		rest := host[i+3:]
		h, path, _ := strings.Cut(rest, "/")
		h = strings.TrimSuffix(h, ":443")
		norm = host[:i+3] + h + "/" + path
		host = h
	}
	d := &pb.Decision{DecisionId: b.id(core.PrefixDecision), ScopeVersion: p.p.CurrentVersion, NormalizedTarget: norm, DecidedAt: b.now(),
		Result: pb.DecisionResult_DECISION_RESULT_DENY, Reason: "no_matching_allow"}
	if p.kill {
		d.Reason = "kill_switch"
		return d, nil
	}
	doc := p.versions[len(p.versions)-1].GetDocument()
	match := func(rl *pb.ScopeRule) bool {
		v := strings.ToLower(rl.GetValue())
		switch rl.GetKind() {
		case pb.RuleKind_RULE_KIND_DOMAIN:
			return host == v
		case pb.RuleKind_RULE_KIND_WILDCARD_DOMAIN:
			return strings.HasSuffix(host, strings.TrimPrefix(v, "*"))
		}
		return false
	}
	for _, rl := range doc.GetRules() {
		if rl.GetEffect() == pb.RuleEffect_RULE_EFFECT_DENY && match(rl) {
			d.Reason, d.RuleId = "explicit_deny", rl.GetId()
			return d, nil
		}
	}
	for _, rl := range doc.GetRules() {
		if rl.GetEffect() == pb.RuleEffect_RULE_EFFECT_ALLOW && match(rl) {
			d.Result, d.Reason, d.RuleId = pb.DecisionResult_DECISION_RESULT_ALLOW, "allow", rl.GetId()
			return d, nil
		}
	}
	return d, nil
}

func (s scopeSrv) SetKillSwitch(ctx context.Context, r *pb.SetKillSwitchRequest) (*pb.KillSwitch, error) {
	b := s.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.GetLevel() != "program" {
		return nil, fail(codes.PermissionDenied, core.ReasonForbiddenRole, "only program kill switches")
	}
	p, err := b.programOf(ctx, r.GetTargetId())
	if err != nil {
		return nil, err
	}
	p.kill = r.GetEnabled()
	p.p.Status = "active"
	if p.kill {
		p.p.Status = "paused"
	}
	return &pb.KillSwitch{Level: "program", TargetId: p.p.Id, Enabled: p.kill, Reason: r.GetReason(), SetBy: from(ctx).sub, SetAt: b.now()}, nil
}

// ---- engine ------------------------------------------------------------

type engineSrv struct {
	pb.UnimplementedEngineServiceServer
	b *Backend
}

func diagnosticsFor(src string) []*pb.Diagnostic {
	if strings.Contains(src, "httpx(Host::") && !strings.Contains(src, "scope.check") {
		i := int32(strings.Index(src, "httpx("))
		return []*pb.Diagnostic{{Code: "E0301", Severity: pb.Diagnostic_SEVERITY_ERROR, Message: "httpx expects InScope<Host> but found Host",
			Primary: &pb.SourceSpan{File: "recon.ff", Start: &pb.SourcePos{Line: 1, Col: i + 1, Offset: i}, End: &pb.SourcePos{Line: 1, Col: i + 23, Offset: i + 22}, Label: "wrap in scope.check(...)?"},
			Help:    "Call scope.check(...)? to obtain an InScope<Host>."}}
	}
	if strings.Contains(src, "syntax error") {
		return []*pb.Diagnostic{{Code: "E0101", Severity: pb.Diagnostic_SEVERITY_ERROR, Message: "unexpected token",
			Primary: &pb.SourceSpan{File: "recon.ff", Start: &pb.SourcePos{Line: 1, Col: 1}, End: &pb.SourcePos{Line: 1, Col: 2, Offset: 1}}}}
	}
	return nil
}

func effectsFor(src string) ([]string, pb.ActivityClass) {
	if strings.Contains(src, "httpx") {
		return []string{"passive", "active"}, pb.ActivityClass_ACTIVITY_CLASS_ACTIVE
	}
	return []string{"passive"}, pb.ActivityClass_ACTIVITY_CLASS_PASSIVE
}

func (e engineSrv) CheckWorkflow(_ context.Context, r *pb.CheckWorkflowRequest) (*pb.CheckWorkflowResponse, error) {
	ds := diagnosticsFor(r.GetSource())
	eff, cls := effectsFor(r.GetSource())
	return &pb.CheckWorkflowResponse{Diagnostics: ds, InferredEffects: eff, InferredClass: cls, Ok: len(ds) == 0}, nil
}

func (e engineSrv) RegisterWorkflow(ctx context.Context, r *pb.RegisterWorkflowRequest) (*pb.WorkflowVersion, error) {
	if ds := diagnosticsFor(r.GetSource()); len(ds) > 0 {
		st := status.New(codes.InvalidArgument, "workflow does not compile")
		details := []protoadapt.MessageV1{protoadapt.MessageV1Of(&errdetails.ErrorInfo{Reason: core.ReasonDSLDiagnostics, Domain: "engine"})}
		for _, d := range ds {
			details = append(details, protoadapt.MessageV1Of(d))
		}
		st2, _ := st.WithDetails(details...)
		return nil, st2.Err()
	}
	b := e.b
	b.mu.Lock()
	defer b.mu.Unlock()
	t := from(ctx).tenant
	if b.workflows[t] == nil {
		b.workflows[t] = map[string][]*pb.WorkflowVersion{}
	}
	vs := b.workflows[t][r.GetName()]
	sum := sha256.Sum256([]byte(r.GetSource()))
	eff, cls := effectsFor(r.GetSource())
	w := &pb.WorkflowVersion{Id: b.id(core.PrefixWorkflowVersion), Name: r.GetName(), Version: int32(len(vs) + 1), Status: "published",
		ModuleSha256: hex.EncodeToString(sum[:]), Effects: eff, InferredClass: cls, Source: r.GetSource(), CreatedAt: b.now(), CreatedBy: from(ctx).sub}
	b.workflows[t][r.GetName()] = append(vs, w)
	return w, nil
}

func (e engineSrv) workflow(ctx context.Context, name string, v int32) (*pb.WorkflowVersion, error) {
	vs := e.b.workflows[from(ctx).tenant][name]
	if len(vs) == 0 {
		return nil, notFound()
	}
	if v == 0 {
		return vs[len(vs)-1], nil
	}
	if v < 0 || int(v) > len(vs) {
		return nil, notFound()
	}
	return vs[v-1], nil
}

func (e engineSrv) GetWorkflowVersion(ctx context.Context, r *pb.GetWorkflowVersionRequest) (*pb.WorkflowVersion, error) {
	e.b.mu.Lock()
	defer e.b.mu.Unlock()
	return e.workflow(ctx, r.GetName(), r.GetVersion())
}

func (e engineSrv) ListWorkflows(ctx context.Context, r *pb.ListWorkflowsRequest) (*pb.ListWorkflowsResponse, error) {
	e.b.mu.Lock()
	defer e.b.mu.Unlock()
	var latest []*pb.WorkflowVersion
	for _, vs := range e.b.workflows[from(ctx).tenant] {
		latest = append(latest, vs[len(vs)-1])
	}
	sortBy(latest, func(w *pb.WorkflowVersion) string { return w.Name })
	items, pg, err := page(latest, r.GetPage())
	if err != nil {
		return nil, err
	}
	return &pb.ListWorkflowsResponse{Latest: items, Page: pg}, nil
}

func (b *Backend) appendEvent(rn *run, typ, actor string, payload map[string]any) {
	st, _ := structpb.NewStruct(payload)
	rn.r.LastSeq++
	rn.events = append(rn.events, &pb.RunEvent{RunId: rn.r.Id, Seq: rn.r.LastSeq, Type: typ, At: b.now(), Actor: actor, Payload: st})
	b.cond.Broadcast()
}

func (e engineSrv) SubmitRun(ctx context.Context, r *pb.SubmitRunRequest) (*pb.Run, error) {
	b := e.b
	c := from(ctx)
	if c.idem == "" {
		return nil, fail(codes.InvalidArgument, core.ReasonValidationFailed, "idempotency-key metadata is required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(r)
	hash := hex.EncodeToString(raw)
	if id, ok := b.runIdem[c.tenant+"|"+c.idem]; ok {
		if b.runs[id].hash != hash {
			return nil, fail(codes.AlreadyExists, core.ReasonIdempotencyKeyReused, "idempotency key reused")
		}
		return b.runs[id].r, nil
	}
	p, err := b.programOf(ctx, r.GetProgramId())
	if err != nil {
		return nil, err
	}
	if p.kill {
		return nil, fail(codes.PermissionDenied, core.ReasonKillSwitchActive, "program kill switch is active", "program_id", p.p.Id)
	}
	w, err := e.workflow(ctx, r.GetWorkflowName(), r.GetWorkflowVersion())
	if err != nil {
		return nil, err
	}
	rn := &run{tenant: c.tenant, hash: hash, r: &pb.Run{Id: b.id(core.PrefixRun), ProgramId: p.p.Id, WorkflowName: w.Name, WorkflowVersion: w.Version,
		ScopeVersion: p.p.CurrentVersion, Status: pb.RunStatus_RUN_STATUS_PENDING, Input: r.GetInput(), CreatedBy: c.sub, CreatedAt: b.now()}}
	b.runs[rn.r.Id] = rn
	b.runIdem[c.tenant+"|"+c.idem] = rn.r.Id
	b.appendEvent(rn, "run_submitted", c.actor, map[string]any{"workflow": w.Name})
	out := proto.Clone(rn.r).(*pb.Run)
	if strings.Contains(w.Source, "approve(") {
		a := &pb.Approval{Id: b.id(core.PrefixApproval), RunId: rn.r.Id, EffectPath: "3", Role: "approver", Reason: "intrusive step",
			Status: "pending", ExpiresAt: timestamppb.New(b.clock.Now().Add(24 * time.Hour)), CreatedAt: b.now()}
		b.approvals[a.Id] = &approval{tenant: c.tenant, a: a}
		rn.r.Status = pb.RunStatus_RUN_STATUS_WAITING
		rn.r.StartedAt = b.now()
		b.appendEvent(rn, "approval_requested", "engine", map[string]any{"approval_id": a.Id, "role": a.Role})
	}
	return out, nil
}

func (e engineSrv) runOf(ctx context.Context, id string) (*run, error) {
	rn, ok := e.b.runs[id]
	if !ok || rn.tenant != from(ctx).tenant {
		return nil, notFound()
	}
	return rn, nil
}

func (e engineSrv) GetRun(ctx context.Context, r *pb.GetRunRequest) (*pb.Run, error) {
	e.b.mu.Lock()
	defer e.b.mu.Unlock()
	rn, err := e.runOf(ctx, r.GetRunId())
	if err != nil {
		return nil, err
	}
	return rn.r, nil
}

func (e engineSrv) ListRuns(ctx context.Context, r *pb.ListRunsRequest) (*pb.ListRunsResponse, error) {
	e.b.mu.Lock()
	defer e.b.mu.Unlock()
	var all []*pb.Run
	for _, rn := range e.b.runs {
		x := rn.r
		if rn.tenant != from(ctx).tenant || (r.GetProgramId() != "" && x.ProgramId != r.GetProgramId()) ||
			(r.GetStatus() != 0 && x.Status != r.GetStatus()) || (r.GetWorkflowName() != "" && x.WorkflowName != r.GetWorkflowName()) {
			continue
		}
		all = append(all, x)
	}
	sortBy(all, func(x *pb.Run) string { return x.Id })
	items, pg, err := page(all, r.GetPage())
	if err != nil {
		return nil, err
	}
	return &pb.ListRunsResponse{Runs: items, Page: pg}, nil
}

func terminal(s pb.RunStatus) bool {
	return s == pb.RunStatus_RUN_STATUS_SUCCEEDED || s == pb.RunStatus_RUN_STATUS_FAILED || s == pb.RunStatus_RUN_STATUS_CANCELLED
}

func (e engineSrv) CancelRun(ctx context.Context, r *pb.CancelRunRequest) (*pb.Run, error) {
	b := e.b
	b.mu.Lock()
	defer b.mu.Unlock()
	rn, err := e.runOf(ctx, r.GetRunId())
	if err != nil {
		return nil, err
	}
	if terminal(rn.r.Status) {
		return nil, fail(codes.FailedPrecondition, core.ReasonRunNotCancellable, "run already finished", "status", rn.r.Status.String())
	}
	rn.r.Status, rn.r.FinishedAt = pb.RunStatus_RUN_STATUS_CANCELLED, b.now()
	b.appendEvent(rn, "run_cancelled", from(ctx).actor, map[string]any{"reason": r.GetReason()})
	return rn.r, nil
}

func (e engineSrv) ListRunEvents(ctx context.Context, r *pb.ListRunEventsRequest) (*pb.ListRunEventsResponse, error) {
	e.b.mu.Lock()
	defer e.b.mu.Unlock()
	rn, err := e.runOf(ctx, r.GetRunId())
	if err != nil {
		return nil, err
	}
	var sel []*pb.RunEvent
	for _, ev := range rn.events {
		if ev.Seq > r.GetAfterSeq() && (len(r.GetTypes()) == 0 || contains(r.GetTypes(), ev.Type)) {
			sel = append(sel, ev)
		}
	}
	items, pg, err := page(sel, r.GetPage())
	if err != nil {
		return nil, err
	}
	return &pb.ListRunEventsResponse{Events: items, Page: pg}, nil
}

func (e engineSrv) WatchRun(r *pb.WatchRunRequest, st grpc.ServerStreamingServer[pb.RunEvent]) error {
	b := e.b
	ctx := st.Context()
	stop := context.AfterFunc(ctx, func() { b.mu.Lock(); b.cond.Broadcast(); b.mu.Unlock() })
	defer stop()
	b.mu.Lock()
	defer b.mu.Unlock()
	rn, err := e.runOf(ctx, r.GetRunId())
	if err != nil {
		return err
	}
	next := r.GetAfterSeq()
	for {
		for _, ev := range rn.events {
			if ev.Seq > next {
				b.mu.Unlock()
				err := st.Send(ev)
				b.mu.Lock()
				if err != nil {
					return err
				}
				next = ev.Seq
			}
		}
		if terminal(rn.r.Status) && next >= rn.r.LastSeq {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b.cond.Wait()
	}
}

// AppendRunEvent appends an event to a run (tests drive watch streams).
func (b *Backend) AppendRunEvent(runID, typ string, payload map[string]any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if rn, ok := b.runs[runID]; ok {
		b.appendEvent(rn, typ, "engine", payload)
	}
}

// FinishRun marks a run succeeded (ends watch streams).
func (b *Backend) FinishRun(runID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if rn, ok := b.runs[runID]; ok {
		rn.r.Status, rn.r.FinishedAt = pb.RunStatus_RUN_STATUS_SUCCEEDED, b.now()
		b.appendEvent(rn, "run_succeeded", "engine", map[string]any{})
	}
}

func (e engineSrv) ListApprovals(ctx context.Context, r *pb.ListApprovalsRequest) (*pb.ListApprovalsResponse, error) {
	e.b.mu.Lock()
	defer e.b.mu.Unlock()
	var all []*pb.Approval
	for _, a := range e.b.approvals {
		x := a.a
		if a.tenant != from(ctx).tenant || (r.GetStatus() != "" && x.Status != r.GetStatus()) || (r.GetRole() != "" && x.Role != r.GetRole()) ||
			(r.GetRunId() != "" && x.RunId != r.GetRunId()) {
			continue
		}
		all = append(all, x)
	}
	sortBy(all, func(x *pb.Approval) string { return x.Id })
	items, pg, err := page(all, r.GetPage())
	if err != nil {
		return nil, err
	}
	return &pb.ListApprovalsResponse{Approvals: items, Page: pg}, nil
}

func (e engineSrv) GetApproval(ctx context.Context, r *pb.GetApprovalRequest) (*pb.Approval, error) {
	e.b.mu.Lock()
	defer e.b.mu.Unlock()
	a, ok := e.b.approvals[r.GetApprovalId()]
	if !ok || a.tenant != from(ctx).tenant {
		return nil, notFound()
	}
	return a.a, nil
}

func (e engineSrv) DecideApproval(ctx context.Context, r *pb.DecideApprovalRequest) (*pb.Approval, error) {
	b := e.b
	b.mu.Lock()
	defer b.mu.Unlock()
	a, ok := b.approvals[r.GetApprovalId()]
	if !ok || a.tenant != from(ctx).tenant {
		return nil, notFound()
	}
	if a.a.Status != "pending" {
		return nil, fail(codes.Aborted, core.ReasonApprovalAlreadyDecided, "approval already decided", "status", a.a.Status)
	}
	rn := b.runs[a.a.RunId]
	if rn.r.CreatedBy == from(ctx).sub {
		return nil, fail(codes.PermissionDenied, core.ReasonSelfApprovalForbidden, "the run's submitter cannot approve it")
	}
	a.a.Status, a.a.DecidedBy, a.a.DecidedAt, a.a.Comment = "approved", from(ctx).sub, b.now(), r.GetComment()
	rn.r.Status = pb.RunStatus_RUN_STATUS_RUNNING
	if r.GetDecision() == pb.ApprovalDecision_APPROVAL_DECISION_DENY {
		a.a.Status = "denied"
		rn.r.Status, rn.r.FinishedAt, rn.r.FailureCode = pb.RunStatus_RUN_STATUS_FAILED, b.now(), "approval_denied"
	}
	b.appendEvent(rn, "approval_decided", from(ctx).actor, map[string]any{"approval_id": a.a.Id, "status": a.a.Status})
	return a.a, nil
}

// ---- tools -------------------------------------------------------------

type toolSrv struct {
	pb.UnimplementedToolRunnerServiceServer
}

func (toolSrv) ListTools(context.Context, *pb.ListToolsRequest) (*pb.ListToolsResponse, error) {
	return &pb.ListToolsResponse{Tools: []*pb.ToolSpec{
		{Name: "httpx", Version: "1.6.0", Description: "HTTP probe", InputType: "InScope<Host>", OutputType: "List<HttpService>",
			Observes: []string{"URL", "Service"}, EffectClass: pb.ActivityClass_ACTIVITY_CLASS_ACTIVE, RateRps: 5, RateBurst: 10, Concurrency: 4,
			Timeout: durationpb.New(30 * time.Second), Idempotent: true, SpecSha256: strings.Repeat("a", 64)},
		{Name: "subfinder", Version: "2.6.0", Description: "Passive subdomain enumeration", InputType: "Fqdn", OutputType: "List<Fqdn>",
			Observes: []string{"FQDN"}, EffectClass: pb.ActivityClass_ACTIVITY_CLASS_PASSIVE, RateRps: 1, RateBurst: 1, Concurrency: 1,
			Timeout: durationpb.New(5 * time.Minute), Idempotent: true, SpecSha256: strings.Repeat("b", 64)},
	}}, nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func sortBy[T any](xs []T, key func(T) string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && key(xs[j]) < key(xs[j-1]); j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}
