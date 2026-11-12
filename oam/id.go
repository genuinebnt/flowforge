package oam

import (
	"crypto/sha256"
	"encoding/binary"

	"github.com/genuinebnt/flowforge/core"
)

// Entity and edge ids are derived from their identity rather than drawn
// at random, so that equal graph states encode to equal bytes no matter in
// which order observations arrived, and so that every graph writer agrees
// on an id without coordination. The UUIDs are RFC 9562 version 8
// (custom) values built from SHA-256 of a length-prefixed tuple.

// EntityUUID returns the id of the entity identified by (tenant, program,
// asset type, key) (spec 10.5 unique constraint).
func EntityUUID(tenantID, programID, assetType, key string) core.UUID {
	return derivedUUID("ff-oam-entity", tenantID, programID, assetType, key)
}

// EntityID returns EntityUUID as a public "ent_" identifier.
func EntityID(tenantID, programID, assetType, key string) string {
	return core.PublicID(core.PrefixEntity, EntityUUID(tenantID, programID, assetType, key))
}

// EdgeUUID returns the id of the edge identified by its upsert key (spec 5.7).
func EdgeUUID(tenantID, programID, fromID, toID, relType, label, discriminator string) core.UUID {
	return derivedUUID("ff-oam-edge", tenantID, programID, fromID, toID, relType, label, discriminator)
}

// EdgeID returns EdgeUUID as a public "edg_" identifier.
func EdgeID(tenantID, programID, fromID, toID, relType, label, discriminator string) string {
	return core.PublicID(core.PrefixEdge, EdgeUUID(tenantID, programID, fromID, toID, relType, label, discriminator))
}

func derivedUUID(domain string, parts ...string) core.UUID {
	h := sha256.New()
	var n [binary.MaxVarintLen64]byte
	for _, p := range append([]string{domain}, parts...) {
		h.Write(n[:binary.PutUvarint(n[:], uint64(len(p)))])
		h.Write([]byte(p))
	}
	var u core.UUID
	copy(u[:], h.Sum(nil))
	u[6] = (u[6] & 0x0f) | 0x80 // version 8
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 9562 variant
	return u
}
