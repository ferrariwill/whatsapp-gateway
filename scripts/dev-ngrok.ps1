# Expõe o WhatsApp Gateway local via ngrok.
# ATENCAO: ngrok gratuito (.ngrok-free.dev) exibe pagina intermediaria que
# impede a verificacao do webhook pela Meta. Prefira: .\scripts\dev-tunnel.ps1
# Uso: .\scripts\dev-ngrok.ps1
# Requer: gateway rodando em $Port (padrão 8082) e ngrok autenticado.

param(
    [int]$Port = 8082,
    [int]$NgrokAPIPort = 4040
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$envFile = Join-Path $root ".env"

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
        $resp = Invoke-WebRequest -Uri "http://127.0.0.1:$port/health" -UseBasicParsing -TimeoutSec 3
        return $resp.StatusCode -eq 200
    } catch {
        return $false
    }
}

function Get-NgrokPublicURL([int]$apiPort) {
    $tunnels = (Invoke-RestMethod -Uri "http://127.0.0.1:$apiPort/api/tunnels").tunnels
    foreach ($t in $tunnels) {
        if ($t.public_url -like "https://*") { return $t.public_url }
    }
    return $null
}

if (-not (Get-Command ngrok -ErrorAction SilentlyContinue)) {
    Write-Error "ngrok não encontrado. Instale em https://ngrok.com/download"
}

if (-not (Test-GatewayHealth $Port)) {
    Write-Host "Gateway não responde em http://127.0.0.1:$Port/health" -ForegroundColor Yellow
    Write-Host "Inicie com F5 (Debug WhatsApp Gateway) ou: cd backend; go run ./cmd/api"
    exit 1
}

$existing = $null
try { $existing = Get-NgrokPublicURL $NgrokAPIPort } catch { }

if ($existing) {
    Write-Host "ngrok já está rodando: $existing" -ForegroundColor Green
} else {
    Write-Host "Iniciando ngrok http $Port ..."
    Start-Process -FilePath "ngrok" -ArgumentList "http", "$Port" -WindowStyle Minimized
    Start-Sleep -Seconds 3
    $existing = Get-NgrokPublicURL $NgrokAPIPort
    if (-not $existing) {
        Write-Error "Falha ao obter URL pública do ngrok. Verifique: ngrok config add-authtoken SEU_TOKEN"
    }
}

$verifyToken = Read-EnvValue "META_WEBHOOK_VERIFY_TOKEN"
if (-not $verifyToken) { $verifyToken = "local-meta-verify-token" }

$webhookURL = "$existing/webhook/whatsapp"
$urlFile = Join-Path $root ".ngrok-webhook-url"
@(
    "public_base=$existing"
    "webhook_url=$webhookURL"
    "verify_token=$verifyToken"
    "updated_at=$(Get-Date -Format o)"
) | Set-Content -Path $urlFile -Encoding UTF8

Write-Host ""
Write-Host "=== Webhook pronto para cadastro na Meta ===" -ForegroundColor Cyan
Write-Host "Callback URL:  $webhookURL"
Write-Host "Verify token:  $verifyToken"
Write-Host ""
Write-Host "Campos webhook: messages (obrigatório)"
Write-Host "Salvo em: $urlFile"
Write-Host ""

try {
    $metaHeaders = @{ "User-Agent" = "facebookplatform/1.0 (+http://developers.facebook.com)" }
    $challenge = Invoke-RestMethod -Uri "$webhookURL`?hub.mode=subscribe&hub.verify_token=$verifyToken&hub.challenge=ngrok-test-ok" -Headers $metaHeaders
    if ($challenge -eq "ngrok-test-ok") {
        Write-Host "Verificação pública OK (GET /webhook/whatsapp)" -ForegroundColor Green
    } else {
        Write-Host "Verificação retornou resposta inesperada: $challenge" -ForegroundColor Yellow
    }
} catch {
    Write-Host "Falha ao testar webhook público: $_" -ForegroundColor Red
}
