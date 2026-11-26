package records

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// fakeQuerier returns one canned tenant row and counts DB lookups.
type fakeQuerier struct {
	calls    int
	schema   string
	vertical string
	features []string
	err      error
}

func (f *fakeQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	f.calls++
	return fakeTenantRow{f}
}

type fakeTenantRow struct{ f *fakeQuerier }

func (r fakeTenantRow) Scan(dest ...any) error {
	if r.f.err != nil {
		return r.f.err
	}
	*dest[0].(*string) = r.f.schema
	*dest[1].(*string) = r.f.vertical
	*dest[2].(*[]string) = r.f.features
	return nil
}

func TestTenantInfoL1CachesAndEvicts(t *testing.T) {
	ResetTenantInfoCacheForTest()
	t.Cleanup(ResetTenantInfoCacheForTest)

	q := &fakeQuerier{schema: "project_centrum", vertical: "sports", features: []string{"records"}}
	const tenant = "l1-test"

	first, err := CheckTenantInfoCached(context.Background(), nil, q, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Active || first.Schema != q.schema || q.calls != 1 {
		t.Fatalf("first=%+v calls=%d", first, q.calls)
	}

	// Same process, same pool, nil Redis: must be an L1 hit, not a DB read.
	second, err := CheckTenantInfoCached(context.Background(), nil, q, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if second.Schema != q.schema || q.calls != 1 {
		t.Fatalf("second=%+v calls=%d, want L1 hit", second, q.calls)
	}

	// Admin eviction must clear L1 immediately on this instance.
	if err := EvictTenantKnown(context.Background(), nil, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckTenantInfoCached(context.Background(), nil, q, tenant); err != nil {
		t.Fatal(err)
	}
	if q.calls != 2 {
		t.Fatalf("after evict calls=%d, want 2", q.calls)
	}

	// TTL expiry falls back to the source.
	key := tenantInfoCacheKey(nil, q, tenant)
	tenantInfoL1Mu.Lock()
	e := tenantInfoL1[key]
	e.expires = time.Now().Add(-time.Second)
	tenantInfoL1[key] = e
	tenantInfoL1Mu.Unlock()
	if _, err := CheckTenantInfoCached(context.Background(), nil, q, tenant); err != nil {
		t.Fatal(err)
	}
	if q.calls != 3 {
		t.Fatalf("after TTL calls=%d, want 3", q.calls)
	}
}

func TestTenantInfoL1CachesNegativeLookup(t *testing.T) {
	ResetTenantInfoCacheForTest()
	t.Cleanup(ResetTenantInfoCacheForTest)

	q := &fakeQuerier{err: pgx.ErrNoRows}
	const tenant = "l1-missing"
	for i := 0; i < 3; i++ {
		info, err := CheckTenantInfoCached(context.Background(), nil, q, tenant)
		if err != nil {
			t.Fatal(err)
		}
		if info.Active {
			t.Fatal("missing tenant must stay inactive")
		}
	}
	if q.calls != 1 {
		t.Fatalf("negative lookup calls=%d, want 1 (cached)", q.calls)
	}
}
