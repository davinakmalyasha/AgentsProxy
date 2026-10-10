// Package tenantembed embeds the tenants/ tree into the binary.
//
// It exists for one mechanical reason: go:embed patterns are relative to
// the source file and may not use "..", so only a file at the repository
// root can cover tenants/. This file has no logic;
// internal/agentconfig does the loading.
//
// Embedding rather than reading from disk at runtime because a per-tenant
// folder that only exists on one machine is a deployment accident, not a
// configuration -- and because reading tenant-controlled paths at runtime
// is the file-traversal class ARCHITECTURE.md already records the CVE for.
package tenantembed

import "embed"

// AgentsFS carries every tenant's declarative folders into the binary.
//
// Only memory/ and skills/ are embedded: that is everything the loader
// reads (project.json, SystemPrompt.md, Information/*.md, skills/*.md).
// The tenant backend/ and tools/ trees are compiled Go — embedding their
// .go sources too shipped ~440KB twice (once compiled, once as text).
//
// The _template tenant is embedded too and skipped by the loader;
// embedding it keeps `go build` honest about its syntax without treating
// it as a live tenant.
//
//go:embed all:tenants/*/memory all:tenants/*/skills
var AgentsFS embed.FS
