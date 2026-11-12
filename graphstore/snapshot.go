package graphstore

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/csr"
	"github.com/genuinebnt/flowforge/oam"
)

// SnapshotInfo is one row of the snapshot catalogue (graph.snapshots,
// spec 21.6).
type SnapshotInfo struct {
	SnapshotID    string
	TenantID      string
	ProgramID     string
	WatermarkSeq  uint64
	NodeCount     int64
	EdgeCount     int64
	URI           string
	SHA256        [sha256.Size]byte
	FormatVersion int
	BuiltAt       time.Time
}

// nodeTypes numbers asset types for the CSR node_type section; 0 is any
// type not listed (opaque or future OAM types). The order is fixed
// forever: new types are appended.
var nodeTypes = []string{
	"",
	oam.TypeFQDN, oam.TypeIPAddress, oam.TypeNetblock, oam.TypeAutonomousSystem,
	oam.TypeDomainRecord, oam.TypeIPNetRecord, oam.TypeAutnumRecord,
	oam.TypeURL, oam.TypeService, oam.TypeTLSCertificate, oam.TypeFile, oam.TypeOrganization,
	oam.TypePerson, oam.TypeLocation, oam.TypePhone, oam.TypeContactRecord, oam.TypeIdentifier,
	oam.TypeAccount, oam.TypeFundsTransfer, oam.TypeProduct, oam.TypeProductRelease,
}

// NodeType returns the CSR node type of an asset type (0 when unlisted).
func NodeType(assetType string) uint8 {
	if i := slices.Index(nodeTypes, assetType); i > 0 {
		return uint8(i)
	}
	return 0
}

// AssetTypeOfNode is the inverse of NodeType ("" for 0 or unknown).
func AssetTypeOfNode(t uint8) string {
	if int(t) < len(nodeTypes) {
		return nodeTypes[t]
	}
	return ""
}

// BuildCSR builds the CSR snapshot of one program (spec 21.2, 21.4).
// Active entities become nodes in id order; active edges whose endpoints
// are both active become edges, with their combined confidence. Stale
// records are excluded, which the header records as
// csr.FlagStaleExcluded. The label dictionary is the sorted set of labels
// used, so equal graphs give byte-identical files. Only ID, AssetType and
// State of entities and FromEntityID, ToEntityID, Label, Confidence and
// State of edges are read.
func BuildCSR(meta csr.Meta, entities []Entity, edges []Edge) (*csr.Graph, error) {
	meta.Flags |= csr.FlagStaleExcluded
	ids := make([]core.UUID, 0, len(entities))
	types := make([]uint8, 0, len(entities))
	index := make(map[string]uint32, len(entities))
	sorted := slices.Clone(entities)
	slices.SortFunc(sorted, func(a, b Entity) int { return cmpString(a.ID, b.ID) })
	for _, e := range sorted {
		if e.State != oam.StateActive {
			continue
		}
		u, err := core.ParsePublicID(core.PrefixEntity, e.ID)
		if err != nil {
			return nil, fmt.Errorf("graphstore: snapshot entity id %q: %w", e.ID, err)
		}
		index[e.ID] = uint32(len(ids))
		ids = append(ids, u)
		types = append(types, NodeType(e.AssetType))
	}
	var labels []string
	for _, e := range edges {
		if e.State == oam.StateActive && !slices.Contains(labels, e.Label) {
			labels = append(labels, e.Label)
		}
	}
	slices.Sort(labels)
	if len(labels) > csr.MaxLabels {
		return nil, core.Errorf(core.KindFailedPrecondition, core.ReasonValidationFailed, "graph uses %d relation labels, the snapshot format holds %d", len(labels), csr.MaxLabels)
	}
	out := make([]csr.Edge, 0, len(edges))
	for _, e := range edges {
		if e.State != oam.StateActive {
			continue
		}
		s, ok1 := index[e.FromEntityID]
		d, ok2 := index[e.ToEntityID]
		if !ok1 || !ok2 {
			continue
		}
		l, _ := slices.BinarySearch(labels, e.Label)
		out = append(out, csr.Edge{Src: s, Dst: d, Label: uint8(l), Conf: float32(e.Confidence)})
	}
	g, err := csr.Build(meta, ids, types, out, labels)
	if err != nil {
		return nil, Internal("build snapshot", err)
	}
	return g, nil
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// SnapshotObjectName is the blob name of a snapshot file (spec 21.4:
// snapshots/{tenant}/{program}/{watermark}.ffcs).
func SnapshotObjectName(tenantID, programID string, watermark uint64) string {
	return "snapshots/" + tenantID + "/" + programID + "/" + strconv.FormatUint(watermark, 10) + ".ffcs"
}

// PublishSnapshot encodes g, writes it through blobs and returns the
// catalogue entry to record. The caller records it.
func PublishSnapshot(ctx context.Context, g *csr.Graph, tenant, program ID, watermark uint64, blobs BlobStore, builtAt time.Time) (SnapshotInfo, error) {
	if blobs == nil {
		return SnapshotInfo{}, InvalidArgument("blobs", "a blob store is required")
	}
	data, err := g.Encode()
	if err != nil {
		return SnapshotInfo{}, Internal("encode snapshot", err)
	}
	uri, err := blobs.Put(ctx, SnapshotObjectName(tenant.Text, program.Text, watermark), data)
	if err != nil {
		return SnapshotInfo{}, Internal("write snapshot", err)
	}
	var sum [sha256.Size]byte
	copy(sum[:], data[len(data)-sha256.Size:])
	return SnapshotInfo{
		SnapshotID: SnapshotUUID(tenant.Text, program.Text, watermark).String(),
		TenantID:   tenant.Text, ProgramID: program.Text, WatermarkSeq: watermark,
		NodeCount: int64(g.NumNodes()), EdgeCount: int64(g.NumEdges()),
		URI: uri, SHA256: sum, FormatVersion: csr.FormatVersion, BuiltAt: StoreTime(builtAt),
	}, nil
}

// SnapshotMeta returns the csr header fields of a snapshot.
func SnapshotMeta(tenant, program ID, watermark uint64, createdAt time.Time) csr.Meta {
	return csr.Meta{TenantID: tenant.UUID, ProgramID: program.UUID, WatermarkSeq: watermark, CreatedAt: createdAt}
}
