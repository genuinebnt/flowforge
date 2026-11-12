package scope

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
)

// Resolver is the name resolution port. Production wires a DNS client;
// tests use StaticResolver. Implementations must honour ctx.
type Resolver interface {
	// LookupAddrs returns the addresses host resolves to.
	LookupAddrs(ctx context.Context, host string) ([]netip.Addr, error)
}

// StaticResolver is the in-memory Resolver fake: it answers from fixed
// maps and never touches the network. The zero value knows no names. It
// is safe for concurrent use as long as the maps are not modified.
type StaticResolver struct {
	// Answers maps a normalized name to its addresses, in answer order.
	Answers map[string][]netip.Addr
	// Errors maps a name to the error its lookup returns.
	Errors map[string]error
}

// LookupAddrs implements Resolver. Unknown names fail with ErrNoSuchHost.
func (r StaticResolver) LookupAddrs(ctx context.Context, host string) ([]netip.Addr, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err, ok := r.Errors[host]; ok {
		return nil, err
	}
	addrs, ok := r.Answers[host]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchHost, host)
	}
	return slices.Clone(addrs), nil
}

// resolveOnce resolves name and canonicalizes the answer: IPv4-mapped
// addresses become IPv4, duplicates are dropped (first occurrence kept).
// A lookup error, an empty answer, a zoned or invalid address, or a nil
// resolver all fail with ErrResolution.
func resolveOnce(ctx context.Context, r Resolver, name string) ([]netip.Addr, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: no resolver configured", ErrResolution)
	}
	raw, err := r.LookupAddrs(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrResolution, name, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: %s: no addresses", ErrResolution, name)
	}
	out := make([]netip.Addr, 0, len(raw))
	for _, a := range raw {
		if !a.IsValid() || a.Zone() != "" {
			return nil, fmt.Errorf("%w: %s: unusable address %q", ErrResolution, name, a)
		}
		a = a.Unmap()
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out, nil
}
