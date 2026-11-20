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
// The legacy Centrum backend still uses Register() and /v1/sports/... so
// its frontend keeps working unchanged.

// ResetForTest clears the global registry. Test-only.
func ResetForTest() { MustInit() }
