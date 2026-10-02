# tools/ — one .go file per agent tool

A new tenant starts with an empty tools/ folder. To add a tool, copy any
file from the centrum tenant's tools/ and adapt it:

- The tool self-registers via `init()` (`agentstools.Register("name",
  constructor)`); nothing else needs to know it exists.
- The tool's JSON Schema travels in the file next to the code that
  implements it (or in the shared meta table).
- Mark state-changing tools `Mutating: true` — boot validation fails
  the build otherwise.
- The package is compiled in through a blank import in cmd/server/main.go,
  next to the other tenants' tools packages.

Role access is structural: each tier's agent is built from a filtered
tool set, so the model never sees a tool its tier was not granted.
