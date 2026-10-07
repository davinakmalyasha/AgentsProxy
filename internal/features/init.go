package features

// DONE: PRD 6.5 (init helpers) / STANDARDS A

// To add a new shared feature:
//  1. drop a package under internal/features/shared/<name>/
//  2. in that package's init() call features.Register(...)
//  3. blank-import it from cmd/server/main.go
//
// To add a custom (per-tenant) feature:
//  1. copy internal/features/_template/ to tenants/<slug>/backend/<name>/
//  2. edit the feature, set VerticalSlugs to the tenant's vertical
//  3. blank-import it from cmd/server/main.go
//
// Registration happens in each package's own init(); there is no
// central Init to call (and no central place to forget).

// ResetForTest clears the registry. Test-only.
func ResetForTest() { MustInit() }
