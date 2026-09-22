param()

$admin = "Bearer local-admin-key-32-bytes-hex-test-1234"
$ts = Get-Date -Format "HHmmss"
$tenantA = "warung-a-$ts"
$tenantB = "warung-b-$ts"

# Create two clients.
$A = Invoke-RestMethod -Method POST -Uri http://localhost:8080/v1/admin/clients `
    -Headers @{Authorization=$admin} -ContentType application/json `
    -Body ("{`"name`":`"warung-a`",`"tenant_id`":`"$tenantA`",`"monthly_quota_tokens`":100000}")
Write-Host "=== A created ===" $A.id $A.tenant_id

$B = Invoke-RestMethod -Method POST -Uri http://localhost:8080/v1/admin/clients `
    -Headers @{Authorization=$admin} -ContentType application/json `
    -Body ("{`"name`":`"warung-b`",`"tenant_id`":`"$tenantB`",`"monthly_quota_tokens`":100000}")
Write-Host "=== B created ===" $B.id $B.tenant_id

# A creates a fnb record.
$create = Invoke-RestMethod -Method POST -Uri http://localhost:8080/v1/fnb/records `
    -Headers @{"X-Client-Token"=$A.token} -ContentType application/json `
    -Body '{"kind":"item","title":"Nasi Goreng Spesial","slug":"nasi-goreng","payload":{"price":25000}}'
Write-Host "=== A POST /v1/fnb/records ===" ($create | ConvertTo-Json -Compress)

# A lists — should have 1.
$listA = Invoke-RestMethod -Method GET -Uri http://localhost:8080/v1/fnb/records `
    -Headers @{"X-Client-Token"=$A.token}
Write-Host "=== A GET /v1/fnb/records === items=" $listA.items.Count " total=" $listA.total
if ($listA.total -ne 1) { Write-Host "FAIL: A total expected 1" -ForegroundColor Red }

# B lists — must be empty.
$listB = Invoke-RestMethod -Method GET -Uri http://localhost:8080/v1/fnb/records `
    -Headers @{"X-Client-Token"=$B.token}
Write-Host "=== B GET /v1/fnb/records === items=" $listB.items.Count " total=" $listB.total
if ($listB.total -ne 0) { Write-Host "FAIL: B total expected 0 (RLS LEAK)" -ForegroundColor Red }

# B gets A's id — must be 404.
$getBstatus = $null
try {
    Invoke-RestMethod -Method GET -Uri ("http://localhost:8080/v1/fnb/records/" + $create.id) -Headers @{"X-Client-Token"=$B.token} | Out-Null
} catch { $getBstatus = $_.Exception.Response.StatusCode.value__ }
Write-Host "=== B GET /v1/fnb/records/<A's id> === status=" $getBstatus
if ($getBstatus -ne 404) { Write-Host "FAIL: B get should be 404" -ForegroundColor Red }

# A get-by-id — 200.
$getA = Invoke-RestMethod -Method GET -Uri ("http://localhost:8080/v1/fnb/records/" + $create.id) -Headers @{"X-Client-Token"=$A.token}
Write-Host "=== A GET /v1/fnb/records/<own id> === title=" $getA.title

# PATCH.
$patch = Invoke-RestMethod -Method PATCH -Uri ("http://localhost:8080/v1/fnb/records/" + $create.id) -Headers @{"X-Client-Token"=$A.token} -ContentType application/json -Body '{"title":"Nasi Goreng Pedas","payload":{"price":28000}}'
Write-Host "=== A PATCH ===" ($patch | ConvertTo-Json -Compress)

# DELETE (soft).
$del = Invoke-RestMethod -Method DELETE -Uri ("http://localhost:8080/v1/fnb/records/" + $create.id) -Headers @{"X-Client-Token"=$A.token}
Write-Host "=== A DELETE ===" ($del | ConvertTo-Json -Compress)

# Confirm soft delete: get returns status=deleted.
$after = Invoke-RestMethod -Method GET -Uri ("http://localhost:8080/v1/fnb/records/" + $create.id) -Headers @{"X-Client-Token"=$A.token}
Write-Host "=== A GET after delete === status=" $after.status
if ($after.status -ne "deleted") { Write-Host "FAIL: status should be 'deleted'" -ForegroundColor Red }

# chat loop (Phase 1 still works).
$devToken = "dev-token"
$chat = Invoke-RestMethod -Method POST -Uri http://localhost:8080/v1/chat -ContentType application/json -Body ("{`"client_token`":`"$devToken`",`"messages`":[{`"role`":`"user`",`"content`":`"halo phase 2`"}]}")
Write-Host "=== POST /v1/chat (dev) === reply=" $chat.reply " usage=" ($chat.usage | ConvertTo-Json -Compress)

Write-Host "=== DONE ===" -ForegroundColor Green
