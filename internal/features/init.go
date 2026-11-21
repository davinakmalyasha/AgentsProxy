package features

// To add a platform (cross-vertical) feature:
//  1. drop a package under internal/features/shared/<name>/
//  2. in that package's init() call features.Register(...)
//  3. blank-import it from cmd/server/main.go
//
// To add a project-specific feature:
//  1. create a package under projects/<slug>/backend/<name>/
//  2. call features.RegisterProject("<slug>", "<vertical>", ...)
//  3. run `go generate ./...` to add the generated blank import
//
// A project feature is scoped to one project and mounted under
// /v1/p/<slug>/...; two projects may own a feature with the same name.
// Centrum dual-registers its features so the legacy /v1/sports/... alias
// remains available as a rollback surface for one release.

// ResetForTest clears both registries. Test-only.
func ResetForTest() {
	MustInit()
	resetProjectRegistryForTest()
}
