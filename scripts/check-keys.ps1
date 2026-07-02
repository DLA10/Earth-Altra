# Verifies your Alpaca keys are valid AND that the SIP / Algo Trader Plus real-time
# data entitlement is active. Reads backend/.env for credentials.
$ErrorActionPreference = "Stop"
$go = "C:\Program Files\Go\bin\go.exe"
Push-Location (Join-Path $PSScriptRoot "..\backend")
try {
    & $go run ./cmd/server -keycheck
} finally {
    Pop-Location
}
