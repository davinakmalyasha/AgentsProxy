param(
    [string]$Root = "D:\MyProject\AgentsProxy"
)

# Map: file glob -> DONE_BLOCK line to insert at the top.
$map = @{
    "cmd\server\main.go"            = "// DONE: PRD 7 (server hardening + graceful shutdown) / STANDARDS A, B, D, H (slog, recovery, Sentry env-gated init, version ldflags)"
    "cmd\server\version.go"         = "// DONE: PRD 6.4 (version from git rev) / STANDARDS B"
    "cmd\mockllm\main.go"           = "// DONE: developer-only helper (Phase 1 verify), used by scripts/verify-phase2.ps1"

    "internal\api\router.go"        = "// DONE: PRD 3 router, 6 API contracts / STANDARDS A, B, F"
    "internal\api\healthz.go"       = "// DONE: PRD 6.4 / STANDARDS H (ping DB+Redis, 500 if down)"
    "internal\api\chat.go"          = "// DONE: PRD 6.1 (chat handler, quota, usage) / STANDARDS A, D, F"
    "internal\api\admin.go"         = "// DONE: PRD 6.3 (admin CRUD) / STANDARDS A, D, F"
    "internal\features\builder.go"      = "// DONE: PRD 6.2 (vertical builder, AllowedVerticals) / STANDARDS A, D, F"
    "internal\api\json.go"          = "// DONE: PRD 6 (json helpers) / STANDARDS B"

    "internal\auth\tokens.go"       = "// DONE: PRD 6.1, 6.3 (GenerateToken, HashToken, ConstantTimeCompare) / STANDARDS D"
    "internal\auth\admin.go"        = "// DONE: PRD 6.3 (RequireAdmin middleware) / STANDARDS A, D"

    "internal\cache\client.go"      = "// DONE: PRD 5 (ck:{hash} cache, 15m TTL) / STANDARDS A, B, E"

    "internal\config\config.go"     = "// DONE: PRD 7 (env config + redaction) / STANDARDS A, D"

    "internal\db\db.go"             = "// DONE: PRD 3 (pgxpool singleton Min=2 Max=20) / STANDARDS A, E"
    "internal\db\migrate.go"        = "// DONE: PRD 4 (idempotent migrations, advisory lock) / STANDARDS A, H"

    "internal\llm\client.go"        = "// DONE: PRD 6.1 (OpenAI-compatible LLM client, 60s timeout, sanitize errors) / STANDARDS A, D"

    "internal\middleware\cors.go"   = "// DONE: PRD 7 (CORS from ALLOW_ORIGINS) / STANDARDS A, D"
    "internal\middleware\logging.go" = "// DONE: PRD 7 (slog + request_id + Sentry breadcrumb) / STANDARDS A, B, H"
    "internal\middleware\ratelimit.go" = "// DONE: PRD 5 (Redis INCR+EXPIRE 60s) / STANDARDS D, E"
    "internal\middleware\recovery.go"  = "// DONE: PRD 7 (panic -> 500 + Sentry) / STANDARDS A, D, H"

    "internal\quota\quota.go"       = "// DONE: PRD 5 (quota:{id}:{month} cached, SUM fallback) / STANDARDS A, B"

    "internal\redisx\client.go"     = "// DONE: PRD 3 (go-redis singleton, scheme auto-prepend for Laragon) / STANDARDS A"

    "internal\sentry\sentry.go"     = "// DONE: PRD H (Sentry env-gated, no-op when DSN empty, PII redaction, panic capture, slog bridge) / STANDARDS H"

    "internal\usage\store.go"       = "// DONE: PRD 6.1, 6.2 (usage row + quota cache incr) / STANDARDS A"
}

# SQL files: insert before first non-comment line.
$sqlMap = @{
    "internal\db\migrations\001_clients_usage.sql" = "-- DONE: PRD 4.1"
    "internal\db\migrations\002_tenancy.sql"    = "-- DONE: PRD 4.2 (RLS PERMISSIVE; see PRD comment for why not RESTRICTIVE)"
    "internal\db\migrations\003_seed_dev.sql"    = "-- DONE: PRD 4.3"
}

foreach ($rel in $map.Keys) {
    $path = Join-Path $Root $rel
    if (-not (Test-Path $path)) {
        Write-Host "skip (not found): $rel"
        continue
    }
    $content = Get-Content $path -Raw
    $marker = $map[$rel]
    if ($content -match [regex]::Escape($marker)) {
        Write-Host "already has DONE_BLOCK: $rel"
        continue
    }
    $new = "$marker`n$content"
    Set-Content -Path $path -Value $new -NoNewline
    Write-Host "added: $rel"
}

foreach ($rel in $sqlMap.Keys) {
    $path = Join-Path $Root $rel
    if (-not (Test-Path $path)) {
        Write-Host "skip (not found): $rel"
        continue
    }
    $content = Get-Content $path -Raw
    $marker = $sqlMap[$rel]
    if ($content -match [regex]::Escape($marker)) {
        Write-Host "already has DONE_BLOCK: $rel"
        continue
    }
    $lines = $content -split "`n"
    $insertAt = 0
    for ($i = 0; $i -lt $lines.Count; $i++) {
        $t = $lines[$i].Trim()
        if ($t -eq "" -or $t.StartsWith("--")) {
            $insertAt = $i + 1
        } else {
            break
        }
    }
    $lines = @($lines[0..($insertAt - 1)]) + @($marker) + @($lines[$insertAt..($lines.Count - 1)])
    Set-Content -Path $path -Value ($lines -join "`n") -NoNewline
    Write-Host "added (sql): $rel"
}
