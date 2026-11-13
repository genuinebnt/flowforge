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
