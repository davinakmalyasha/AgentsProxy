package features

// DONE: PRD 6.5 (init helpers) / STANDARDS A

// Init is the canonical feature-registration entry point. Called
// once from main.go after every shared/custom feature package has
// had a chance to call features.Register.
//
// To add a new shared feature:
//  1. drop a package under internal/features/shared/<name>/
//  2. in that package's init() call features.Register(...)
//  3. blank-import it from cmd/server/main.go
//
// To add a custom (per-tenant) feature:
//  1. copy internal/features/_template/ to tenants/<slug>/backend/<name>/
//  2. edit the feature, set VerticalSlugs to the tenant's vertical
//  3. blank-import it from cmd/server/main.go
func Init() {
	// The shared feature packages register themselves in their own
	// init() functions. This Init() exists for symmetry and as a
	// future hook for promotion or cross-cutting features.
}

// ResetForTest clears the registry. Test-only.
func ResetForTest() { MustInit() }
