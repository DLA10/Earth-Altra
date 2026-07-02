# Starts the Vite dev server on :5173 (proxies /api and /ws to the backend on :8080).
$ErrorActionPreference = "Stop"
Push-Location (Join-Path $PSScriptRoot "..\frontend")
try {
    npm run dev
} finally {
    Pop-Location
}
