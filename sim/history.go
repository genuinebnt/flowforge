package sim

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/scope"
)

// History is everything a scenario records for the invariant checker
// (spec 25.7): the store's lease grants and commits in linearization
// order, the final event log of every run, every call the fake tool
// executor saw, the limiter's admissions, the scope documents, the
// observations the graph ingester accepted and the resulting graph, and
// the alerts the notifier produced. Every field is exported so tests can
// hand-craft violating histories.
type History struct {
	Seed     uint64 `json:"seed"`
	Profile  string `json:"profile"`
	Workload string `json:"workload"`

	Start time.Time `json:"start"`
	// FaultsEnd is when fault injection stopped (Scenario.Duration after
	// Start); End is when the simulation stopped.
	FaultsEnd time.Time `json:"faults_end"`
	End       time.Time `json:"end"`
	// LivenessBound is how long after FaultsEnd every run must be
	// terminal (inv_11).
	LivenessBound time.Duration `json:"liveness_bound"`

	Leases  []LeaseGrant `json:"leases"`
	Commits []Commit     `json:"commits"`
	Runs    []RunRecord  `json:"runs"`

	ToolCalls  []ToolCall  `json:"tool_calls"`
	Admissions []Admission `json:"admissions"`
	// EdgeAdmissions are the per-target admissions of the network edge
	// (the egress proxy's limiter, V2_DESIGN §9), one per request that
	// passed it, duplicates included.
	EdgeAdmissions []Admission   `json:"edge_admissions"`
	RateCeilings   []RateCeiling `json:"rate_ceilings"`
	// KillSwitches is the timeline of kill switch changes; KillSwitchRaces
	// are the calls the edge refused because a kill switch was engaged
	// after the engine's re-check (reported, not violations).
	KillSwitches    []KillSwitchChange `json:"kill_switches"`
	KillSwitchRaces []KillSwitchRace   `json:"kill_switch_races"`

	Scopes []ScopeVersion `json:"scopes"`

	Observations []Observation `json:"observations"`
	Graphs       []GraphState  `json:"graphs"`

	AlertRules []AlertRule   `json:"alert_rules"`
	Alerts     []AlertRecord `json:"alerts"`

	// Compensations names forward/undo tool pairs whose undo calls must
	// run in reverse order of the forward results (inv_07).
	Compensations []CompensationPair `json:"compensations"`

	Submissions []Submission `json:"submissions"`
	Modules     []Module     `json:"-"`

	Stats Stats `json:"stats"`
}

// LeaseGrant is one successful lease acquisition (AcquireLease or
// ClaimReady). Op is the store's linearization index.
type LeaseGrant struct {
	Op    uint64           `json:"op"`
	At    time.Time        `json:"at"`
	Run   string           `json:"run"`
	Owner string           `json:"owner"`
	Actor uint64           `json:"actor"`
	Fence eventstore.Fence `json:"fence"`
}

// Commit is one successful append (or the initial events of CreateRun,
// with a zero fence), covering seq FromSeq..ToSeq.
type Commit struct {
	Op      uint64           `json:"op"`
	At      time.Time        `json:"at"`
	Run     string           `json:"run"`
	Owner   string           `json:"owner"`
	Actor   uint64           `json:"actor"`
	Fence   eventstore.Fence `json:"fence"`
	FromSeq uint64           `json:"from_seq"`
	ToSeq   uint64           `json:"to_seq"`
}

// RunRecord is a run the cluster created and its final log.
type RunRecord struct {
	ID              string             `json:"id"`
	Workflow        string             `json:"workflow"`
	WorkflowVersion string             `json:"workflow_version"`
	Tenant          string             `json:"tenant"`
	Program         string             `json:"program"`
	IdempotencyKey  string             `json:"idempotency_key"`
	Input           json.RawMessage    `json:"input"`
	Status          string             `json:"status"`
	Log             []eventstore.Event `json:"log"`
	// OpenEnded marks a run the workload left waiting on purpose; inv_11
	// does not require it to be terminal.
	OpenEnded bool `json:"open_ended,omitempty"`
}

// ToolCall is one request delivered to (or lost on the way to) the fake
// tool executor.
type ToolCall struct {
	Seq          uint64          `json:"-"` // executor arrival order (not deterministic within a par_map round)
	At           time.Time       `json:"at"`
	Actor        uint64          `json:"actor"`
	Worker       string          `json:"worker"`
	Run          string          `json:"run"`
	Path         string          `json:"path"`
	Attempt      int             `json:"attempt"`
	Tool         string          `json:"tool"`
	Class        string          `json:"class"`
	Key          string          `json:"key"`
	Input        json.RawMessage `json:"input"`
	Target       string          `json:"target,omitempty"`    // normalized host of an InScope input
	LimitKey     string          `json:"limit_key,omitempty"` // rl:h key the call is charged against
	ScopeVersion int32           `json:"scope_version"`
	DecisionIDs  []string        `json:"decision_ids"`
	// Delivered is false for a request lost before reaching the tool.
	// Duplicate marks a second delivery of the same request by the
	// transport.
	Delivered bool `json:"delivered"`
	Duplicate bool `json:"duplicate,omitempty"`
	// EdgeRefused is the network edge's refusal of a request it did not
	// pass on: "rate_limited" or "kill_switch" (Delivered is then false).
	EdgeRefused string `json:"edge_refused,omitempty"`
}

// KillSwitchChange is one kill switch engaged or released: level global,
// tenant or program, and the tenant or program id it applies to.
type KillSwitchChange struct {
	At      time.Time `json:"at"`
	Level   string    `json:"level"`
	ID      string    `json:"id,omitempty"`
	Engaged bool      `json:"engaged"`
}

// KillSwitchRace is a call the engine dispatched after its re-check and
// the network edge refused because a kill switch was engaged meanwhile.
type KillSwitchRace struct {
	At    time.Time `json:"at"`
	Run   string    `json:"run"`
	Path  string    `json:"path"`
	Tool  string    `json:"tool"`
	Level string    `json:"level"`
}

// Admission is one per-target key admitted by a limiter: the engine's
// (rl:h, Actor the calling worker) or the network edge's (Actor 0).
type Admission struct {
	At    time.Time `json:"at"`
	Actor uint64    `json:"actor"`
	Key   string    `json:"key"`
}

// RateCeiling is the configured per-target ceiling of keys with Prefix.
type RateCeiling struct {
	Prefix string  `json:"prefix"`
	Rate   float64 `json:"rate"`
	Burst  int     `json:"burst"`
}

// ScopeVersion is one published scope document.
type ScopeVersion struct {
	Tenant  string         `json:"tenant"`
	Program string         `json:"program"`
	Version int32          `json:"version"`
	Doc     scope.Document `json:"doc"`
}

// Observation is one observation envelope the graph ingester accepted.
type Observation struct {
	ID          string          `json:"id"`
	Tenant      string          `json:"tenant"`
	Program     string          `json:"program"`
	Run         string          `json:"run"`
	Path        string          `json:"path"`
	Tool        string          `json:"tool"`
	ToolVersion string          `json:"tool_version"`
	Raw         json.RawMessage `json:"raw"`
}

// GraphState is the canonical export of one program's graph after the
// simulation (and after every Pub/Sub delivery was made).
type GraphState struct {
	Tenant  string `json:"tenant"`
	Program string `json:"program"`
	Export  string `json:"export"`
}

// AlertRule describes an alert rule the notifier ran: the run event type
// it matches and its throttle.
type AlertRule struct {
	ID        string        `json:"id"`
	EventType string        `json:"event_type"` // run event type, e.g. alert_emitted
	Throttle  time.Duration `json:"throttle"`
}

// AlertRecord is one alert the notifier produced (delivered unless
// Suppressed by the throttle).
type AlertRecord struct {
	At             time.Time `json:"at"`
	Rule           string    `json:"rule"`
	DedupeKey      string    `json:"dedupe_key"`
	TriggerEventID string    `json:"trigger_event_id"`
	Run            string    `json:"run"`
	Seq            uint64    `json:"seq"`
	Suppressed     bool      `json:"suppressed,omitempty"`
}

// CompensationPair names a forward tool and the tool that undoes it.
type CompensationPair struct {
	Forward string `json:"forward"`
	Undo    string `json:"undo"`
}

// Submission is one SubmitRun call of the workload's client.
type Submission struct {
	At       time.Time `json:"at"`
	Key      string    `json:"key"`
	Workflow string    `json:"workflow"`
	Run      string    `json:"run,omitempty"`
	Err      string    `json:"err,omitempty"`
}

// Module is a registered workflow version, for replay (inv_05).
type Module struct {
	VersionID string
	Name      string
	Encoded   []byte
	Tools     []engine.ToolSpec
}

// Stats counts what happened in a scenario.
type Stats struct {
	Steps          uint64 `json:"steps"`
	Runs           int    `json:"runs"`
	Succeeded      int    `json:"succeeded"`
	Failed         int    `json:"failed"`
	Cancelled      int    `json:"cancelled"`
	Events         int    `json:"events"`
	StoreCalls     int    `json:"store_calls"`
	Crashes        int    `json:"crashes"`
	Partitions     int    `json:"partitions"`
	FencedAppends  int    `json:"fenced_appends"`
	ToolDeliveries int    `json:"tool_deliveries"`
	ToolReruns     int    `json:"tool_reruns"`
	PubDeliveries  int    `json:"pub_deliveries"`
	PubDuplicates  int    `json:"pub_duplicates"`
	Alerts         int    `json:"alerts"`
	Observations   int    `json:"observations"`
	// EdgeDenials are requests the edge's per-target limit refused;
	// KillSwitchRaces is len(History.KillSwitchRaces).
	EdgeDenials     int `json:"edge_denials"`
	KillSwitchRaces int `json:"kill_switch_races"`
}

// Violation is one broken invariant.
type Violation struct {
	Inv string `json:"inv"` // inv_01 .. inv_11
	Run string `json:"run,omitempty"`
	Msg string `json:"msg"`
}

// String returns "<inv> run <run>: <msg>", or "<inv>: <msg>" for a
// violation not tied to one run.
func (v Violation) String() string {
	if v.Run != "" {
		return fmt.Sprintf("%s run %s: %s", v.Inv, v.Run, v.Msg)
	}
	return v.Inv + ": " + v.Msg
}

// Hash returns the hex SHA-256 of the history's canonical form: two runs
// of the same scenario must produce the same hash (spec 25.7 "a failing
// seed reproduces exactly"). Tool calls are hashed in a canonical order
// because the executions of one par_map round arrive concurrently.
func (h *History) Hash() string {
	calls := slices.Clone(h.ToolCalls)
	slices.SortStableFunc(calls, compareCalls)
	c := *h
	c.ToolCalls = calls
	sum := sha256.New()
	enc := json.NewEncoder(sum)
	if err := enc.Encode(c); err != nil {
		_, _ = io.WriteString(sum, "unencodable: "+err.Error())
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func compareCalls(a, b ToolCall) int {
	if c := a.At.Compare(b.At); c != 0 {
		return c
	}
	if a.Actor != b.Actor {
		if a.Actor < b.Actor {
			return -1
		}
		return 1
	}
	if c := strings.Compare(a.Key, b.Key); c != 0 {
		return c
	}
	if a.Attempt != b.Attempt {
		return a.Attempt - b.Attempt
	}
	if a.Duplicate != b.Duplicate {
		if !a.Duplicate {
			return -1
		}
		return 1
	}
	return 0
}
