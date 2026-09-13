$ErrorActionPreference = "Stop"
$DataDir = Join-Path $env:TEMP "tbg-playwright-e2e"
Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $DataDir
New-Item -ItemType Directory -Force -Path $DataDir | Out-Null

$env:DATA_DIR = $DataDir
$env:LISTEN_ADDR = "127.0.0.1:18081"
$env:PORT = "18081"
$env:RUNTIME_ROLE = "monolith-dev"

Write-Host "Starting gateway in background on 127.0.0.1:18081..."
$gatewayProc = Start-Process -FilePath "go" -ArgumentList "run", "./cmd/gateway" -PassThru

try {
    $client = New-Object System.Net.Http.HttpClient
    $healthy = $false
    for ($i = 0; $i -lt 30; $i++) {
        Start-Sleep -Milliseconds 500
        try {
            $resp = $client.GetAsync("http://127.0.0.1:18081/healthz").Result
            if ($resp.IsSuccessStatusCode) {
                $healthy = $true
                break
            }
        } catch {}
    }

    if (-not $healthy) {
        throw "Gateway did not start within 15 seconds"
    }

    Write-Host "Gateway healthy! Running Playwright E2E tests..."
    Set-Location -Path "./web"
    $env:E2E_SKIP_WEBSERVER = "1"
    $env:E2E_BASE_URL = "http://127.0.0.1:18081"
    bunx playwright test
}
finally {
    Write-Host "Stopping gateway process..."
    if ($gatewayProc) {
        Stop-Process -Id $gatewayProc.Id -Force -ErrorAction SilentlyContinue
    }
    Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $DataDir
}
