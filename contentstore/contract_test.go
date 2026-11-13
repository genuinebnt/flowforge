package contentstore_test

import (
	"testing"

	"github.com/genuinebnt/flowforge/contentstore"
	"github.com/genuinebnt/flowforge/contentstore/storetest"
)

// TestCStoreContract_Mem runs the Repo contract against the in-memory fake.
func TestCStoreContract_Mem(t *testing.T) {
	storetest.Run(t, func(*testing.T) contentstore.Repo { return contentstore.NewMemRepo() })
}

func memRepo(*testing.T) contentstore.Repo { return contentstore.NewMemRepo() }

// TestCStoreInv14_RederiveIdempotent: CSTORE-INV-14 on MemRepo
// (storetest.RederiveIdempotent; contentstore/pg runs it on Postgres).
func TestCStoreInv14_RederiveIdempotent(t *testing.T) { storetest.RederiveIdempotent(t, memRepo) }

// TestCStoreInv15_RederiveNoChange: CSTORE-INV-15 on MemRepo
// (storetest.RederiveNoChange).
func TestCStoreInv15_RederiveNoChange(t *testing.T) { storetest.RederiveNoChange(t, memRepo) }

// TestCStoreInv16_RederiveEqualsFreshIngest: CSTORE-INV-16 on MemRepo
// (storetest.RederiveEqualsFresh).
func TestCStoreInv16_RederiveEqualsFreshIngest(t *testing.T) {
	storetest.RederiveEqualsFresh(t, memRepo)
}

// TestRegress_RederiveUnchangedSuccessorDiff: the diff into a snapshot
// whose re-derived records do not change is still recomputed when its
// predecessor's did (storetest.RederiveUnchangedSuccessor).
func TestRegress_RederiveUnchangedSuccessorDiff(t *testing.T) {
	storetest.RederiveUnchangedSuccessor(t, memRepo)
}
