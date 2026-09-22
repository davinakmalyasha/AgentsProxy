Set-Location "D:\MyProject\AgentsProxy"
. .\scripts\load-env.ps1
Write-Host "RATE_LIMIT=$env:RATE_LIMIT_PER_MINUTE"
# Tee all output to proxy.log so diagnostics survive after the window closes.
go run ./cmd/server *>&1 | Tee-Object -FilePath "D:\MyProject\AgentsProxy\proxy.log" -Append
