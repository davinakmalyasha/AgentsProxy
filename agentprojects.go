// Package agentprojects embeds the four per-tenant folders into the binary.
//
// It exists for one mechanical reason: go:embed patterns are relative to
// the source file and may not use "..", so only a file at the repository
// root can cover AgentsTools, AgentsSkills, AgentsMcp and AgentsMemory.
// This file has no logic; internal/agentconfig does the loading.
//
// Embedding rather than reading from disk at runtime because a per-tenant
// folder that only exists on one machine is a deployment accident, not a
// configuration -- and because reading tenant-controlled paths at runtime
// is the file-traversal class ARCHITECTURE.md already records the CVE for.
package agentprojects

import "embed"

// AgentsFS carries every tenant's agent folders into the binary.
//
// Underscore-prefixed template directories are embedded too and skipped by
// the loader; embedding them keeps `go build` honest about their syntax
// without treating them as live tenants.
//
//go:embed all:AgentsTools all:AgentsSkills all:AgentsMcp all:AgentsMemory
var AgentsFS embed.FS
