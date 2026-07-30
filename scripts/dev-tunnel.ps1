# Expõe o gateway via Cloudflare Tunnel (recomendado para webhook Meta).
# O ngrok gratuito exibe pagina intermediaria que quebra a verificacao da Meta.
# Uso: .\scripts\dev-tunnel.ps1

param(
    [int]$Port = 8082
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$envFile = Join-Path $root ".env"
$cloudflared = "C:\Program Files (x86)\cloudflared\cloudflared.exe"
if (-not (Test-Path $cloudflared)) {
    $cloudflared = (Get-Command cloudflared -ErrorAction SilentlyContinue).Source
}
if (-not $cloudflared) {
    Write-Error "cloudflared nao encontrado. Instale: winget install Cloudflare.cloudflared"
}

function Read-EnvValue([string]$key) {
    if (-not (Test-Path $envFile)) { return "" }
    foreach ($line in Get-Content $envFile) {
        if ($line -match "^\s*$key=(.*)$") {
            return $matches[1].Trim().Trim('"').Trim("'")
        }
    }
    return ""
}

function Test-GatewayHealth([int]$port) {
    try {
        return (Invoke-WebRequest -Uri "http://127.0.0.1:$port/health" -UseBasicParsing -TimeoutSec 3).StatusCode -eq 200
    } catch { return $false }
}

if (-not (Test-GatewayHealth $Port)) {
    Write-Host "Gateway nao responde em http://127.0.0.1:$Port/health" -ForegroundColor Yellow
    Write-Host "Inicie com F5 (Debug WhatsApp Gateway) ou: cd backend; go run ./cmd/api"
    exit 1
}

$logFile = Join-Path $root ".cloudflared.log"
$urlFile = Join-Path $root ".webhook-tunnel-url"

# Encerra cloudflared anterior do projeto (se existir)
Get-CimInstance Win32_Process -Filter "Name='cloudflared.exe'" -ErrorAction SilentlyContinue |
    Where-Object { $_.CommandLine -like "*127.0.0.1:$Port*" } |
    ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }

Start-Process -FilePath $cloudflared -ArgumentList @(
    "tunnel", "--url", "http://127.0.0.1:$Port", "--no-autoupdate"
) -RedirectStandardOutput $logFile -RedirectStandardError $logFile -WindowStyle Hidden

$publicBase = $null
for ($i = 0; $i - 30; $i++) {
    Start-Sleep -Seconds 1
    if (Test-Path $logFile) {
        $match = Select-String -Path $logFile -Pattern "https://[a-z0-9-]+\.trycloudflare\.com" | Select-Object -Last 1
        if ($match) {
            $publicBase = $match.Matches[0].Value
            break
        }
    }
}

if (-not $publicBase) {
    Write-Error "Nao foi possivel obter URL do Cloudflare Tunnel. Veja $logFile"
}

$verifyToken = Read-EnvValue "META_WEBHOOK_VERIFY_TOKEN"
if (-not $verifyToken) { $verifyToken = "local-meta-verify-token" }

$webhookURL = "$publicBase/webhook/whatsapp"
@(
    "public_base=$publicBase"
    "webhook_url=$webhookURL"
    "verify_token=$verifyToken"
    "tunnel=cloudflare"
    "updated_at=$(Get-Date -Format o)"
) | Set-Content -Path $urlFile -Encoding UTF8

Write-Host ""
Write-Host "=== Webhook Meta (Cloudflare Tunnel) ===" -ForegroundColor Cyan
Write-Host "Callback URL:  $webhookURL"
Write-Host "Verify token:  $verifyToken"
Write-Host "Campos:        messages"
Write-Host "Salvo em:      $urlFile"
Write-Host ""

$challenge = Invoke-RestMethod -Uri "$webhookURL`?hub.mode=subscribe&hub.verify_token=$verifyToken&hub.challenge=tunnel-test-ok"
if ($challenge -eq "tunnel-test-ok") {
    Write-Host "Verificacao publica OK" -ForegroundColor Green
} else {
    Write-Host "Resposta inesperada: $challenge" -ForegroundColor Yellow
}
