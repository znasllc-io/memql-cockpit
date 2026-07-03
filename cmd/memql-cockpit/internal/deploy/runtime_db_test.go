package deploy

// D1 (memql#2377): DB-backed embedded-runtime init. Skips without a DSN so
// the default CI lane is unaffected; run against a seeded/blank Postgres via
// MEMQL_DATABASE_DSN to exercise the real path (migrate-on-start included).

import (
	"os"
	"testing"
)

func TestEmbeddedRuntime_DBBackedInit(t *testing.T) {
	if os.Getenv("MEMQL_DATABASE_DSN") == "" {
		t.Skip("MEMQL_DATABASE_DSN not set; DB-backed runtime init not exercised")
	}
	rt := NewEmbeddedRuntime(nil).(*embeddedRuntime)
	if err := rt.init(); err != nil {
		t.Fatalf("DB-backed init failed: %v", err)
	}
	if !rt.dbBacked {
		t.Fatal("runtime must report dbBacked with MEMQL_DATABASE_DSN set")
	}
	if err := rt.Resolve("deployEngineCluster"); err != nil {
		t.Fatalf("deployEngineCluster must resolve on the DB-backed runtime: %v", err)
	}
}
