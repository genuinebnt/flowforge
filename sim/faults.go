package sim

import (
	"fmt"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// FaultProfile parameterizes the fault adapters (spec 25.7 fault
// profiles). Probabilities are per call; every draw comes from the
// scenario's seeded generator. Faults are injected only until
// Scenario.Duration has elapsed; afterwards the cluster runs fault-free
// so that liveness (inv_11) can be checked.
type FaultProfile struct {
	Name string

	// BaseLatency is the upper bound of the uniform latency of every
	// store, scope, ingest and publish call.
	BaseLatency time.Duration
	// SpikeP is the probability that a call takes SpikeMin..SpikeMax
	// instead (slow profile); tools spike with the same probability.
	SpikeP             float64
	SpikeMin, SpikeMax time.Duration

	// DropRequestP: the call never reaches the service; the caller sees a
	// timeout after Timeout. DropResponseP: the call is executed but the
	// reply is lost (timeout). DuplicateP: the call is executed twice.
	// FailP: the service answers with a transient error.
	DropRequestP, DropResponseP, DuplicateP, FailP float64
	// Timeout is how long a caller waits for a dropped call.
	Timeout time.Duration

	// ToolFailP is the probability of a transient tool error; ToolDropP of
	// a lost request, ToolLostReplyP of a lost reply (the tool ran),
	// ToolDupP of a duplicated delivery (the tool runs twice).
	ToolFailP, ToolDropP, ToolLostReplyP, ToolDupP float64

	// CrashP is the probability that a worker's process dies at one of
	// its store calls (before or after the call took effect). CrashEvery
	// adds crashes at random instants (exponential with this mean; 0:
	// none). RestartAfter bounds the restart delay.
	CrashP       float64
	CrashEvery   time.Duration
	RestartAfter time.Duration

	// PartitionEvery is the mean time between partitions of one worker
	// from the store; PartitionFor bounds their length.
	PartitionEvery time.Duration
	PartitionFor   time.Duration

	// MaxSkew bounds the per-worker wall-clock offset (both signs).
	MaxSkew time.Duration

	// PubDupP is the probability that a Pub/Sub message is delivered
	// again (up to PubMaxCopies copies); PubDelay bounds the delivery
	// delay, which reorders messages.
	PubDupP      float64
	PubMaxCopies int
	PubDelay     time.Duration
}

// Profile names (spec 25.7).
const (
	ProfileNone        = "none"
	ProfileCrashy      = "crashy"
	ProfileLossy       = "lossy"
	ProfileSlow        = "slow"
	ProfilePartitioned = "partitioned"
	ProfileSkewed      = "skewed"
	ProfileChaos       = "chaos"
)

// ProfileNames lists the profiles in a fixed order.
var ProfileNames = []string{ProfileNone, ProfileCrashy, ProfileLossy, ProfileSlow, ProfilePartitioned, ProfileSkewed, ProfileChaos}

// Profile returns the named fault profile.
func Profile(name string) (FaultProfile, error) {
	base := FaultProfile{Name: name, BaseLatency: 20 * time.Millisecond, Timeout: 5 * time.Second,
		RestartAfter: 20 * time.Second, PubMaxCopies: 1, PubDelay: 200 * time.Millisecond}
	switch name {
	case ProfileNone:
		base.BaseLatency = 0
		base.PubDelay = 0
	case ProfileCrashy:
		base.CrashP = 0.01
		base.CrashEvery = 3 * time.Minute
	case ProfileLossy:
		base.DropRequestP, base.DropResponseP, base.DuplicateP, base.FailP = 0.02, 0.02, 0.03, 0.02
		base.ToolFailP, base.ToolDropP, base.ToolLostReplyP, base.ToolDupP = 0.05, 0.03, 0.03, 0.05
		base.PubDupP, base.PubMaxCopies, base.PubDelay = 0.3, 5, 30*time.Second
	case ProfileSlow:
		base.SpikeP, base.SpikeMin, base.SpikeMax = 0.02, 2*time.Second, 45*time.Second
		base.PubDelay = 20 * time.Second
	case ProfilePartitioned:
		base.PartitionEvery, base.PartitionFor = 4*time.Minute, 90*time.Second
	case ProfileSkewed:
		base.MaxSkew = 90 * time.Second
	case ProfileChaos:
		base.CrashP, base.CrashEvery = 0.005, 5*time.Minute
		base.DropRequestP, base.DropResponseP, base.DuplicateP, base.FailP = 0.01, 0.01, 0.02, 0.01
		base.ToolFailP, base.ToolDropP, base.ToolLostReplyP, base.ToolDupP = 0.03, 0.02, 0.02, 0.03
		base.SpikeP, base.SpikeMin, base.SpikeMax = 0.01, 2*time.Second, 40*time.Second
		base.PartitionEvery, base.PartitionFor = 6*time.Minute, 60*time.Second
		base.MaxSkew = 60 * time.Second
		base.PubDupP, base.PubMaxCopies, base.PubDelay = 0.3, 5, 30*time.Second
	default:
		return FaultProfile{}, fmt.Errorf("sim: unknown fault profile %q (want one of %v)", name, ProfileNames)
	}
	return base, nil
}

// callPlan is what the fault adapter does with one call.
type callPlan struct {
	dropRequest  bool
	dropResponse bool
	duplicate    bool
	fail         bool
	crashBefore  bool
	crashAfter   bool
}

func chance(r core.Rng, p float64) bool { return p > 0 && core.Float64(r) < p }

func between(r core.Rng, lo, hi time.Duration) time.Duration {
	if hi <= lo {
		return lo
	}
	return lo + time.Duration(r.Uint64()%uint64(hi-lo))
}

// latency draws the latency of one call.
func (p FaultProfile) latency(r core.Rng, faulty bool) time.Duration {
	if faulty && chance(r, p.SpikeP) {
		return between(r, p.SpikeMin, p.SpikeMax)
	}
	if p.BaseLatency <= 0 {
		return 0
	}
	return between(r, 0, p.BaseLatency)
}

// plan draws the faults of one call. Crashes apply only to worker calls.
func (p FaultProfile) plan(r core.Rng, worker bool) callPlan {
	var c callPlan
	switch {
	case worker && chance(r, p.CrashP):
		if r.Uint64()%2 == 0 {
			c.crashBefore = true
		} else {
			c.crashAfter = true
		}
	case chance(r, p.DropRequestP):
		c.dropRequest = true
	case chance(r, p.FailP):
		c.fail = true
	case chance(r, p.DropResponseP):
		c.dropResponse = true
	case chance(r, p.DuplicateP):
		c.duplicate = true
	}
	return c
}
