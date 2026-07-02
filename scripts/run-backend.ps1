# Starts the Live-Optimus backend (SIP ingest + WebSocket hub + REST API) on :8080.
$ErrorActionPreference = "Stop"
$go = "C:\Program Files\Go\bin\go.exe"
Push-Location (Join-Path $PSScriptRoot "..\backend")
try {
    & $go run ./cmd/server
} finally {
    Pop-Location
}
