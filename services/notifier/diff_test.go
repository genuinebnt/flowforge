package notifier_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/notifier"
)

// TestNotifierDiffFromContent: a content.changes event becomes the alert
// `diff` section field for field, and every text is redacted again before
// it can reach an alert, even when the producer let a credential through.
func TestNotifierDiffFromContent(t *testing.T) {
	// Built at run time: a format-valid token must not sit in the source.
	token := "ghp_" + strings.Repeat("aB3dE5fG7h", 3) + "J9kL0m"
	pl, err := structpb.NewStruct(map[string]any{
		"snapshot_id": "snp_2", "snapshot_from": "snp_1", "diff_id": "dif_2", "url": "https://shop.example/app.js", "profile": "default",
		"outcome": "changed", "levels": []any{"hash", "text", "semantic"}, "class": "semantic_endpoint", "newness": "likely_new",
		"cause": "target", "alertable": true, "score": 0.75,
		"score_features": []any{map[string]any{"name": "new_endpoint", "weight": 0.4}},
		"changes":        []any{map[string]any{"op": "added", "kind": "endpoint", "key": "ep:POST https://shop.example/api/" + token}},
		"excerpt":        "@@ -1 +1,2 @@\n+const t = \"" + token + "\";\n",
		"observation":    "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	env := &pb.EventEnvelope{EventId: "0190a1b2-0000-7000-8000-000000000001", Type: "content.changed", TenantId: "ten_x",
		Payload: &pb.EventEnvelope_Generic{Generic: pl}}
	ev := notifier.EventFromEnvelope(env)
	d := ev.Diff
	if d == nil || ev.Change == nil || ev.Change.Type != "content_changed" {
		t.Fatalf("event %+v", ev)
	}
	if d.SnapshotID != "snp_2" || d.FromSnapshotID != "snp_1" || d.DiffID != "dif_2" || d.URL != "https://shop.example/app.js" ||
		d.Outcome != "changed" || !d.Semantic() || d.Class != "semantic_endpoint" || d.Newness != "likely_new" || d.Cause != "target" ||
		!d.Alertable || d.Score != 0.75 || len(d.ScoreFeatures) != 1 || d.ScoreFeatures[0].Name != "new_endpoint" ||
		len(d.Changes) != 1 || !strings.HasPrefix(d.Changes[0], "added endpoint|ep:POST https://shop.example/api/") {
		t.Fatalf("diff %+v", d)
	}
	for _, s := range append([]string{d.Excerpt}, d.Changes...) {
		if strings.Contains(s, token) || !strings.Contains(s, "[REDACTED:github_token:") {
			t.Fatalf("not redacted: %q", s)
		}
	}
	if ev.Diff.Kinds()[0] != "endpoint_added" {
		t.Fatalf("kinds %v", ev.Diff.Kinds())
	}
	graph := notifier.EventFromEnvelope(&pb.EventEnvelope{EventId: "0190a1b2-0000-7000-8000-000000000002", Type: "graph.entity_added", TenantId: "ten_x",
		Payload: &pb.EventEnvelope_GraphChange{GraphChange: &pb.ChangeEvent{Type: "entity_added"}}})
	if graph.Diff != nil {
		t.Fatalf("a graph event got a diff: %+v", graph.Diff)
	}
}
