package gatewayd_test

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/devstack"
	"github.com/genuinebnt/flowforge/services/gateway/gatewayd"
	"github.com/genuinebnt/flowforge/testkit"
)

// TestGatewayContentRoutes: the REST views of the content service (V2
// §11 item 14): GET /v1/snapshots/{id}, /v1/snapshots/{a}/diff/{b} in
// unified and side-by-side JSON (and "previous"), /v1/changes/search,
// first appeared, pinning and quota; another tenant gets 404.
func TestGatewayContentRoutes(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the whole stack")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &logBuffer{}
	log := slog.New(slog.NewJSONHandler(io.MultiWriter(logs), &slog.HandlerOptions{Level: slog.LevelDebug}))
	addr := freeAddr(t)
	issuer := "http://" + addr
	gw, err := gatewayd.New(ctx, gatewayd.Config{Addr: addr, Issuer: issuer, PublicURL: issuer, CookieInsec: true, ShutdownGrace: time.Second}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	st, err := devstack.Start(ctx, devstack.Options{Issuer: issuer, TrustKeys: gw.Keys(), Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	backends := map[string]string{
		"FF_SCOPE_ADDR": st.Addrs[devstack.ScopeName], "FF_ENGINE_ADDR": st.Addrs[devstack.EngineName],
		"FF_GRAPH_ADDR": st.Addrs[devstack.GraphName], "FF_NOTIFIER_ADDR": st.Addrs[devstack.NotifierName],
		"FF_TOOL_RUNNER_ADDR": st.Addrs[devstack.ToolRunnerName], "FF_SIMILARITY_ADDR": st.Addrs[devstack.SimilarityName],
		"FF_CONTENT_ADDR": st.Addrs[devstack.ContentName],
	}
	gw.UseBus(st.Bus)
	served := make(chan error, 1)
	go func() { served <- gw.Serve(ctx, backends) }()
	defer func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	c := &client{t: t, base: "http://" + addr + "/v1"}
	c.waitReady("http://" + addr + "/readyz")

	login := func(email string) (string, string) {
		signup := c.do("POST", "/auth/signup", map[string]any{"email": email, "password": "a long enough passphrase 42", "tenant_name": email}, 202)
		tenant := signup["tenant_id"].(string)
		token := c.waitFor(func() (string, bool) {
			ms := regexp.MustCompile(`token=([A-Za-z0-9_-]+)`).FindAllStringSubmatch(logs.String(), -1)
			if len(ms) == 0 {
				return "", false
			}
			return ms[len(ms)-1][1], true
		})
		c.do("POST", "/auth/verify-email", map[string]any{"token": token}, 200)
		l := c.do("POST", "/auth/login", map[string]any{"email": email, "password": "a long enough passphrase 42", "tenant_id": tenant}, 200)
		return tenant, l["access_token"].(string)
	}
	tenant, tokenA := login("content@example.com")
	c.bearer = tokenA
	prog := c.do("POST", "/programs", map[string]any{"name": "Shop", "scope": map[string]any{
		"rules":    []any{map[string]any{"id": "r1", "effect": "allow", "kind": "domain", "value": "shop.example"}},
		"activity": map[string]any{"max_class": "active", "windows": []any{}}}}, 201)
	pid := prog["id"].(string)

	// Captures arrive over gRPC (the collect tool's path).
	tu, err := core.ParsePublicID(core.PrefixTenant, tenant)
	if err != nil {
		t.Fatal(err)
	}
	ictx := st.As(ctx, tu, "service:tool-runner", auth.RoleAnalyst)
	var ids []string
	for i, v := range []string{"v1", "v2", "v3"} {
		var hs []*pb.HttpHeader
		for _, l := range strings.Split(strings.TrimSpace(string(testkit.ReadFile(t, "content/sites/spa", v+".headers.txt"))), "\n") {
			k, val, _ := strings.Cut(l, ":")
			hs = append(hs, &pb.HttpHeader{Name: strings.TrimSpace(k), Value: strings.TrimSpace(val)})
		}
		r, err := st.Content.IngestCapture(ictx, &pb.IngestCaptureRequest{ProgramId: pid, Capture: &pb.Capture{Url: "https://shop.example/",
			Status: 200, Headers: hs, Body: testkit.ReadFile(t, "content/sites/spa", v+".index.html"),
			FetchedAt: timestamppb.New(time.Date(2026, 11, 8+7*i, 10, 0, 0, 0, time.UTC))}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.GetSnapshot().GetId())
	}

	sn := c.do("GET", "/snapshots/"+ids[2]+"?include=body,features", nil, 200)
	if sn["id"] != ids[2] || !strings.Contains(sn["body"].(string), "[REDACTED:") || sn["features"] == nil || sn["parent_id"] != ids[1] {
		t.Fatalf("snapshot: %v", sn)
	}
	d := c.do("GET", "/snapshots/"+ids[1]+"/diff/"+ids[2]+"?format=unified", nil, 200)
	arts, _ := d["artifacts"].([]any)
	if d["format"] != "unified" || len(arts) == 0 || d["change"].(map[string]any)["outcome"] != "changed" {
		t.Fatalf("unified diff: %v", d)
	}
	for _, a := range arts {
		if _, ok := a.(map[string]any)["side_by_side"]; ok {
			t.Fatal("side-by-side in a unified view")
		}
	}
	sbs := c.do("GET", "/snapshots/previous/diff/"+ids[2]+"?format=side_by_side", nil, 200)
	found := false
	for _, a := range sbs["artifacts"].([]any) {
		if h, ok := a.(map[string]any)["side_by_side"].(map[string]any); ok && len(h["hunks"].([]any)) > 0 {
			found = true
		}
	}
	if !found || sbs["from_snapshot_id"] != ids[1] {
		t.Fatalf("side-by-side diff: %v", sbs)
	}
	c.do("GET", "/snapshots/"+ids[1]+"/diff/"+ids[2]+"?format=nope", nil, 400)
	hits := c.do("GET", "/changes/search?program_id="+pid+"&q="+"kind:form_added", nil, 200)
	if hs, _ := hits["hits"].([]any); len(hs) == 0 {
		t.Fatalf("search: %v", hits)
	}
	c.do("GET", "/changes/search?program_id="+pid+"&q=%28", nil, 400)
	fa := c.do("GET", "/changes/first-appeared?program_id="+pid+"&text=/account/delete", nil, 200)
	if fa["snapshot_id"] != ids[2] {
		t.Fatalf("first appeared: %v", fa)
	}
	pin := c.do("POST", "/snapshots/"+ids[0]+"/:pin", map[string]any{"reason": "reviewed"}, 200)
	if pin["pinned"] != true {
		t.Fatalf("pin: %v", pin)
	}
	q := c.do("GET", "/content/quota", nil, 200)
	if q["usage"].(map[string]any)["captures"].(float64) != 3 {
		t.Fatalf("quota: %v", q)
	}
	list := c.do("GET", "/snapshots?program_id="+pid, nil, 200)
	if items, _ := list["items"].([]any); len(items) != 3 {
		t.Fatalf("list: %v", list)
	}

	// Another tenant: nothing exists.
	_, tokenB := login("other@example.com")
	c.bearer = tokenB
	c.do("GET", "/snapshots/"+ids[2], nil, 404)
	c.do("GET", "/snapshots/"+ids[1]+"/diff/"+ids[2], nil, 404)
}
