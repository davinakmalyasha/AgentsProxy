Set-Location "D:\MyProject\AgentsProxy"
$env:LLM_BASE_URL="http://localhost:18080"
$env:LLM_API_KEY="mock-key"
$env:LLM_MODEL="mock-model"
$env:DATABASE_URL="postgres://app:app@localhost:5432/agents_proxy?sslmode=disable"
$env:DATABASE_ADMIN_URL="postgres://postgres:postgres@localhost:5432/agents_proxy?sslmode=disable"
$env:REDIS_URL="localhost:6379"
$env:ADMIN_API_KEY="local-admin-key-32-bytes-hex-test-1234"
go run ./cmd/server
