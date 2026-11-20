// Package projectembed embeds the projects/ tree into the binary.
//
// It exists for one mechanical reason: go:embed patterns are relative to
// the source file and may not use "..", so only a file at the repository
// root can cover projects/. This file has no logic; internal/agentconfig
// and internal/db do the loading.
//
// Only declarative project files are embedded: project.json, the agent
// persona/skills/MCP config, and project migrations. The project backend
// and agent tools are compiled Go, not embedded as source.
package projectembed

import "embed"

// FS carries every project's declarative files into the binary.
//
//go:embed all:projects/*/project.json
//go:embed all:projects/*/agent/agent.md all:projects/*/agent/skills all:projects/*/agent/mcp.json
//go:embed all:projects/*/migrations
var FS embed.FS
