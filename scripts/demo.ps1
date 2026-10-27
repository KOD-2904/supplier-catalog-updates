# PowerShell Demo Script for Supplier Catalog Updates
$ErrorActionPreference = "Stop"

Write-Host "=== 1. Building Binaries ===" -ForegroundColor Cyan
go build -o bin/catalogsync.exe ./cmd/catalogsync
go build -o bin/mocksupplier.exe ./cmd/mocksupplier

$mockProc = $null

function Cleanup {
    if ($mockProc -and !$mockProc.HasExited) {
        Write-Host "Stopping mock supplier..." -ForegroundColor Yellow
        Stop-Process -Id $mockProc.Id -Force -ErrorAction SilentlyContinue
    }
}

try {
    Write-Host "`n=== 2. Starting Mock Supplier (Normal Scenario) ===" -ForegroundColor Cyan
    $mockProc = Start-Process -FilePath ".\bin\mocksupplier.exe" -ArgumentList "-port 8080 -scenario normal" -PassThru
    Start-Sleep -Seconds 1

    Remove-Item "catalog_demo.db*" -Force -ErrorAction SilentlyContinue

    Write-Host "`n=== 3. First Sync Run (Start) ===" -ForegroundColor Cyan
    .\bin\catalogsync.exe -db catalog_demo.db start

    Write-Host "`n=== 4. Checking Status & Catalog Products ===" -ForegroundColor Cyan
    .\bin\catalogsync.exe -db catalog_demo.db status
    .\bin\catalogsync.exe -db catalog_demo.db products -status active | Select-Object -First 15

    Write-Host "`n=== 5. Testing Idempotency (Running exact same feed again) ===" -ForegroundColor Cyan
    .\bin\catalogsync.exe -db catalog_demo.db start

    Write-Host "`n=== 6. Simulating Supplier Outage & Resumability ===" -ForegroundColor Cyan
    Stop-Process -Id $mockProc.Id -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 1

    $mockProc = Start-Process -FilePath ".\bin\mocksupplier.exe" -ArgumentList "-port 8080 -scenario normal -outage-after 2 -outage-for 5m" -PassThru
    Start-Sleep -Seconds 1

    Remove-Item "outage_demo.db*" -Force -ErrorAction SilentlyContinue
    Write-Host "Running sync until outage..." -ForegroundColor Yellow

    try {
        .\bin\catalogsync.exe -db outage_demo.db -retries 2 -backoff 100ms start
    } catch {
        Write-Host "Expected outage error occurred." -ForegroundColor DarkYellow
    }

    Write-Host "`nRun was interrupted as expected. Checking status:" -ForegroundColor Cyan
    .\bin\catalogsync.exe -db outage_demo.db status

    Write-Host "`nSimulating supplier recovery (POST /admin/recover)..." -ForegroundColor Cyan
    Invoke-RestMethod -Uri "http://localhost:8080/admin/recover" -Method Post | Out-Null

    Write-Host "`nResuming interrupted run..." -ForegroundColor Cyan
    .\bin\catalogsync.exe -db outage_demo.db resume
    .\bin\catalogsync.exe -db outage_demo.db status

    Write-Host "`n=== Demo Completed Successfully ===" -ForegroundColor Green
} finally {
    Cleanup
}

