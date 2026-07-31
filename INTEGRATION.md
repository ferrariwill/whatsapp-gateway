# Manual de Integração — WhatsApp Gateway

Este documento descreve como conectar **qualquer aplicação** (sistema de agendamento, CRM, ERP, etc.) ao nosso Gateway multi-tenant de WhatsApp (Meta Cloud API).

O Gateway organiza o tráfego em dois níveis:

| Conceito | Descrição |
|---|---|
| **Aplicação (System)** | Tenant raiz cadastrado no Gateway. Possui uma `X-API-Key` e opcionalmente uma `webhook_url` de retorno. |
| **Cliente externo (External Client)** | Sub-tenant dentro da aplicação (ex.: ID `45`, slug `clinica-centro`). Cada um pode ter um chip WhatsApp (`phone_number_id`) vinculado. |

**Base URL (exemplos):**

| Ambiente | URL |
|---|---|
| Local | `http://localhost:8080` |
| Produção (Render) | `https://seu-gateway.onrender.com` |

---

## 1. Configuração inicial

### 1.1 Infraestrutura do Gateway (operador)

Credenciais da Meta ficam **apenas no servidor**, nunca no painel por cliente:

| Variável | Descrição |
|---|---|
| `META_GLOBAL_TOKEN` | System User Permanent Access Token (conta corporativa WABA) |
| `META_WABA_ID` | WhatsApp Business Account ID (criação/listagem de templates) |
| `META_WEBHOOK_VERIFY_TOKEN` | Token de verificação do webhook Meta |
| `MOTHER_SYSTEM_WEBHOOK_URL` | URL de fallback para repassar respostas inbound (se `systems.webhook_url` estiver vazio). **Não** se aplica a fan-out de status. |
| `STATUS_CALLBACK_MAX_ATTEMPTS` | Tentativas de callback de status antes de DLQ (default: `8`) |
| `MONTHLY_MESSAGE_LIMIT` | Limite mensal de disparos por cliente externo (default: `3000`) |
| `JWT_SECRET` | Sessão do painel administrativo |
| `DATABASE_URL` | PostgreSQL (Supabase ou local) |

Consulte `.env.example` na raiz do monorepo.

### 1.2 Cadastro no painel administrativo

1. Acesse `GET /login` (ex.: `https://seu-gateway.onrender.com/login`).
2. Faça login com o usuário administrador.
3. **Aplicação mãe:** deve existir ao menos um registro em `systems` (API Key + webhook opcional). Em dev local, use `scripts/seed-local.sql`.
4. **Vincular canal:** clique em **Vincular canal** e informe:
   - **Rótulo do canal** — identificação interna (ex.: `Canal principal`)
   - **ID do cliente externo** — identificador do tenant na sua aplicação (ex.: `45`)
   - **Número de telefone** — chip WhatsApp, formato internacional sem `+` (ex.: `5511999999999`)
   - **Phone Number ID da Meta** — ID do chip no Meta Business Manager

> **Segurança:** a `X-API-Key` da aplicação é armazenada apenas como hash SHA-256. O token Meta **não** vai para o banco — somente em variáveis de ambiente do backend.

### 1.3 Cadastro via API (opcional)

```
POST /v1/channels
X-API-Key: sk_live_...
Content-Type: application/json
```

```json
{
  "salon_name": "Canal principal",
  "external_client_id": "45",
  "phone_number_id": "123456789012345",
  "whatsapp_phone_number": "5511999999999"
}
```

O campo `salon_name` é o rótulo legível do canal (legado no schema; use como label interno).

### 1.4 O que a aplicação integradora recebe

| Item | Uso |
|---|---|
| `X-API-Key` | Autenticação em todas as rotas `/v1/*` |
| `external_client_id` | Identifica qual chip/cliente usar no disparo |
| `webhook_url` da aplicação | URL que receberá POST quando o usuário responder no WhatsApp |
| Template aprovado na Meta | Nome exato em `template_name` |

### Headers obrigatórios na API

| Header | Valor |
|---|---|
| `Content-Type` | `application/json` |
| `X-API-Key` | `sk_live_xxxxxxxx...` |

### 1.5 Embedded Signup — state OAuth assinado

Para conectar o WhatsApp de um tenant, o Gateway exige um `state` **assinado e de uso único**. Monte-o pelo endpoint abaixo; o formato legado `{slug}_{tenant_id}` é recusado por padrão (`EMBEDDED_SIGNUP_ALLOW_UNSIGNED_STATE=false`) e devolve `state OAuth assinado obrigatório`.

```
POST /v1/embedded-signup/state
```

Autenticação: `X-API-Key` da aplicação. O `state` emitido vale para o system dono da chave — não é possível emitir para outra aplicação.

**Body**

| Campo | Tipo | Obrigatório | Descrição |
|---|---|---|---|
| `tenant_id` | string | sim | Identificador do tenant no seu SaaS |
| `sistema_origem` | string | não | Se enviado, precisa ser igual ao slug da aplicação autenticada |
| `ttl_seconds` | int | não | Validade do state. Padrão **30 min**; valores acima de **2 h** são reduzidos ao teto |

```json
{
  "tenant_id": "45",
  "ttl_seconds": 900
}
```

**Resposta (`201 Created`)**

```json
{
  "state": "eyJzbHVnIjoi...<assinado>",
  "expires_at": "2026-07-30T20:00:00Z",
  "tenant_id": "45",
  "slug": "beleza_web"
}
```

Use `state` como parâmetro do Embedded Signup da Meta. Ele é consumido no callback: um segundo uso, um state expirado ou um tenant divergente falham com `state OAuth inválido, expirado ou já utilizado`.

**Erros**

| Status | Quando |
|---|---|
| `400` | body inválido ou `tenant_id` ausente |
| `401` | `X-API-Key` ausente ou inválida |
| `403` | `sistema_origem` diferente do slug da aplicação autenticada |
| `500` | segredo do state (`OAUTH_STATE_SECRET` / `META_APP_SECRET`) não configurado no Gateway |

Os nonces emitidos ficam em `oauth_state_nonces`. O Gateway coleta os expirados periodicamente (`OAUTH_STATE_NONCE_GC_INTERVAL`, padrão 1 h), preservando-os por uma janela de diagnóstico de replay (`OAUTH_STATE_NONCE_RETENTION`, padrão 24 h após o vencimento).

---

## 2. Fluxo de disparo (enviar mensagem)

A aplicação chama o Gateway informando **qual cliente externo** deve enviar. O Gateway usa o `phone_number_id` vinculado a esse cliente e o `META_GLOBAL_TOKEN` do servidor.

### Endpoint

```
POST /v1/messages/send-template
```

### Parâmetros do body (JSON)

| Campo | Tipo | Obrigatório | Descrição |
|---|---|:---:|---|
| `external_client_id` | `string` | Sim | ID do cliente/tenant na sua aplicação (ex.: `45`) |
| `phone_number` | `string` | Sim | Telefone do destinatário, internacional sem `+` (ex.: `5511999887766`) |
| `template_name` | `string` | Sim | Nome do template aprovado na Meta |
| `appointment_id` | `string` | Sim | Referência do evento no seu sistema (log/auditoria no Gateway) |
| `variables` | `string[]` | Não* | Valores para `{{1}}`, `{{2}}`… do corpo do template |

\* Obrigatório se o template tiver variáveis no body.

### Exemplo de payload

```json
{
  "external_client_id": "45",
  "phone_number": "5511999887766",
  "template_name": "lembrete_agendamento",
  "appointment_id": "apt-2026-0616-001",
  "variables": [
    "Maria Silva",
    "16/06/2026",
    "14:30"
  ]
}
```

### Resposta de sucesso (`200 OK`)

```json
{
  "message_log_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "status": "sent"
}
```

### Respostas de erro comuns

| HTTP | Significado |
|---|---|
| `401` | `X-API-Key` ausente ou inválida |
| `400` | JSON inválido ou campos obrigatórios faltando |
| `404` | Nenhum canal WhatsApp cadastrado para o `external_client_id` |
| `429` | Limite mensal (`MONTHLY_MESSAGE_LIMIT`) excedido para este cliente externo |
| `502` | Meta rejeitou o envio; log salvo com `status: failed` |

### Exemplo em Go (`net/http`)

```go
payload := map[string]any{
  "external_client_id": "45",
  "phone_number":       "5511999887766",
  "template_name":      "lembrete_agendamento",
  "appointment_id":     "apt-2026-0616-001",
  "variables":          []string{"Maria Silva", "16/06/2026", "14:30"},
}
// POST para /v1/messages/send-template com header X-API-Key
```

### SDK do monorepo (Go)

```go
import "github.com/whatsappgetway/gateway/pkg/whatsapp"

client := whatsapp.NewClient("https://seu-gateway.onrender.com", os.Getenv("GATEWAY_API_KEY"))
client.TemplateName = "lembrete_agendamento"

err := client.SendAppointmentConfirmation(ctx,
  "45",                    // external_client_id
  "apt-2026-0616-001",     // appointment_id
  "5511999887766",         // telefone do cliente final
  []string{"Maria Silva", "16/06/2026", "14:30"},
)
```

---

## 3. Fluxo de retorno (resposta do usuário → sua aplicação)

Quando o usuário toca em um botão ou envia texto, a Meta notifica o Gateway em:

```
GET|POST /webhooks/meta/{phone_number_id}
```

O Gateway identifica a **aplicação** e o **cliente externo** pelo `phone_number_id` do chip e repassa o evento somente para a `webhook_url` da conexão ou da aplicação. Sem destino configurado, a entrega falha explicitamente.

### Arquitetura

```text
┌─────────────────┐   POST /v1/messages/send-template    ┌──────────────────┐
│   Aplicação     │   X-API-Key + external_client_id     │ WhatsApp Gateway │
│  (System)       │ ────────────────────────────────────► │                  │
└─────────────────┘                                       └────────┬─────────┘
        ▲                                                            │
        │                                                            │ META_GLOBAL_TOKEN
        │                                                            ▼
        │                                                   ┌──────────────────┐
        │                                                   │   Meta WhatsApp  │
        │                                                   └────────┬─────────┘
        │   POST webhook_url (JSON abaixo)                           │
        └──────────────────────────────────────────────────────────┘
```

### Payload enviado para a sua `webhook_url`

```
POST https://api.sua-aplicacao.com/webhooks/whatsapp-gateway
Content-Type: application/json
```

```json
{
  "system_id": "uuid-da-aplicacao-no-gateway",
  "external_client_id": "45",
  "meta_message_id": "wamid.HBgLMT...",
  "phone_number": "5511999887766",
  "text": "APPT_CONFIRM",
  "event_type": "button_reply",
  "action": "CONFIRM"
}
```

| Campo | Descrição |
|---|---|
| `system_id` | UUID da aplicação no Gateway |
| `external_client_id` | Cliente externo dono do chip (path legado); no webhook unificado o campo equivalente é `tenant_id` |
| `meta_message_id` | Identidade imutável do evento na Meta — use como chave de idempotência |
| `phone_number` | WhatsApp de quem respondeu |
| `text` | Texto ou payload do botão |
| `event_type` | `text_message` ou `button_reply` |
| `action` | `CONFIRM` ou `CANCEL` quando for resposta de botão de agendamento; vazio para texto livre |

### Valores de `action`

| `action` | Payload Meta | Uso sugerido |
|---|---|---|
| `CONFIRM` | `APPT_CONFIRM` | Confirmar agendamento |
| `CANCEL` | `APPT_RESCHEDULE` | Cancelar / reagendar |

> **Correlação com agendamento:** o webhook **não** inclui `appointment_id`. Preferir `meta_message_id` para deduplicar; na ausência dele, correlacionar por `external_client_id`/`tenant_id` + `phone_number` (e janela de tempo ou estado pendente).

### Resposta esperada

Responda `200 OK` rapidamente:

```json
{"ok": true}
```

### Reentrega de repasses interrompidos (`replay`)

Se o Gateway for reiniciado no meio de um repasse, a linha de auditoria fica em
`pending`. Um sweep periódico **claima** a linha (`pending` → `relaying`) antes
do POST — duas instâncias não podem repassar a mesma linha. Claims órfãos
(processo morto no meio do POST) voltam a ser elegíveis após o TTL do lease.

O corpo do replay traz `"replay": true`, a chave idempotente `meta_message_id`
(a mesma do repasse em tempo real) e as duas chaves de identidade do tenant
(`tenant_id` e `external_client_id`), porque a linha não guarda por qual
endpoint o evento entrou. A resolução de destino é a mesma do tempo real:
webhook da conexão → webhook do system. Não há fallback compartilhado entre tenants.

```json
{
  "system_id": "uuid-da-aplicacao-no-gateway",
  "sistema_origem": "beleza_web",
  "tenant_id": "45",
  "external_client_id": "45",
  "meta_message_id": "wamid.HBgLMT...",
  "phone_number": "5511999887766",
  "text": "APPT_CONFIRM",
  "event_type": "button_reply",
  "action": "CONFIRM",
  "replay": true
}
```

Trate o consumo como idempotente pela chave `meta_message_id`: um evento pode
chegar em tempo real e depois como `replay` se a confirmação do primeiro
repasse não chegou a ser gravada (ou se um claim órfão foi reclaimado).

---

## 3.1 Fan-out de status de entrega (`message.status`)

Quando a Meta envia `statuses[]` (`sent` / `delivered` / `read` / `failed`), o
Gateway persiste o evento em `message_delivery_events` (idempotente por
`meta_message_id` + `status` + `timestamp`) e faz POST assíncrono ao callback
do produto.

**Resolução de URL (sem fallback mother):** `connection.webhook_url` →
`systems.webhook_url`. Se ambas vazias, o evento é gravado com
`callback_status=failed` e **não** há POST (não usa `MOTHER_SYSTEM_WEBHOOK_URL`).

**Secret HMAC:** cada conexão tem `webhook_secret` (gerado automaticamente no
create/update admin se vazio — 32 bytes hex). Nunca logado. Use-o para validar
o header `X-Gateway-Signature-256`.

### Payload

```
POST {callback_url}
Content-Type: application/json
X-Gateway-Signature-256: sha256=<hex HMAC-SHA256(body, webhook_secret)>
X-Gateway-Event: message.status
User-Agent: WhatsAppGateway/1.0
```

```json
{
  "event_type": "message.status",
  "tenant_id": "45",
  "product_id": "beleza_web",
  "system_id": "uuid-da-aplicacao-no-gateway",
  "meta_message_id": "wamid.HBgLMT...",
  "recipient": "5511999887766",
  "status": "delivered",
  "timestamp": "2026-07-31T12:00:00Z",
  "errors": []
}
```

| Campo | Descrição |
|---|---|
| `status` | `sent`, `delivered`, `read` ou `failed` |
| `errors` | Espelho do `errors[]` Meta quando `status=failed`; senão `[]` |
| `timestamp` | Timestamp Meta em RFC3339 |

Retries curtos (0 / 200ms / 500ms / 1s); depois agenda `next_retry_at` (1m → 5m
→ 15m → 1h). Após `STATUS_CALLBACK_MAX_ATTEMPTS` (default 8) → `dlq`.
4xx do SaaS (exceto 429) vão direto para DLQ após 1 tentativa.

Reprocessar DLQ (admin JWT):

```
POST /admin/delivery-events/{id}/reprocess
```

### Consulta de status

```
GET /v1/messages/{meta_message_id}/status
X-API-Key: sk_live_...
```

- `200` — lista de eventos do **seu** `system_id`, ordenada por timestamp Meta
- `404` — id inexistente em qualquer system
- `403` — id existe só em outro system (sem vazamento cross-tenant)

---

## 4. Auditoria de volume (API)

Consulta consumo mensal de um cliente externo (controle de margem / abuso):

```
GET /v1/usage/report?external_client_id=45&month=6&year=2026
X-API-Key: sk_live_...
```

### Resposta (`200 OK`)

```json
{
  "system_id": "uuid-da-aplicacao",
  "system_name": "Sistema Local de Teste",
  "external_client_id": "45",
  "month": 6,
  "year": 2026,
  "within_monthly_limit": true,
  "monthly_limit": 3000,
  "usage": {
    "total_messages_sent": 120,
    "total_delivered": 115,
    "total_failed": 5,
    "estimated_meta_cost": 0.7820
  }
}
```

| Campo | Descrição |
|---|---|
| `within_monthly_limit` | `true` se `total_messages_sent <= monthly_limit` |
| `estimated_meta_cost` | Estimativa interna (USD) com base em mensagens UTILITY entregues |

O painel admin exibe o mesmo agrupamento em **Volume de Disparos por Aplicação e Clientes** (`GET /admin/usage/volume`, HTMX).

---

## 5. Referência rápida — rotas

| Método | Rota | Auth | Descrição |
|---|---|---|---|
| `POST` | `/v1/messages/send-template` | `X-API-Key` | Dispara template |
| `GET` | `/v1/messages/{meta_message_id}/status` | `X-API-Key` | Histórico de status de entrega (scoped) |
| `POST` | `/v1/channels` | `X-API-Key` | Cadastra canal WhatsApp para um cliente externo |
| `POST` | `/v1/embedded-signup/state` | `X-API-Key` | Emite o state OAuth assinado single-use do Embedded Signup |
| `POST` | `/v1/templates` | `X-API-Key` | Cria template na Meta e upsert local `PENDING` |
| `POST` | `/v1/templates/sync` | `X-API-Key` | Sync Graph → catálogo local (`tenant_id`) |
| `GET` | `/v1/templates` | `X-API-Key` | Lista catálogo local (`?tenant_id=`) |
| `GET` | `/v1/templates/{name}` | `X-API-Key` | Detalhe local (`?tenant_id=&language=`) |
| `POST` | `/admin/templates/sync` | JWT (cookie) | Sync admin (`system_id` + `tenant_id`) |
| `GET` | `/v1/usage/report` | `X-API-Key` | Relatório de volume mensal por cliente externo |
| `GET` | `/health` | — | Health check |
| `GET` | `/login` | — | Painel administrativo |
| `POST` | `/admin/channels` | JWT (cookie) | Vincula canal via painel |
| `POST` | `/admin/delivery-events/{id}/reprocess` | JWT (cookie) | Reenvia evento de status em DLQ |
| `GET` | `/admin/usage/volume` | JWT (cookie) | Fragmento HTML de volume (HTMX) |
| `GET/POST` | `/webhooks/meta/{phone_number_id}` | Meta | Webhook inbound da Meta |
| `GET/POST` | `/webhook/whatsapp` | Meta | Webhook unificado (status + inbound + template management) |

Envio de template (`/send-notification`, `/v1/messages/send-template`) valida o catálogo local **antes** da Graph: `422` com `code` `template_not_found` | `template_not_approved` | `param_mismatch`.
---

## 6. Checklist de integração

- [ ] Aplicação cadastrada em `systems` com `X-API-Key` segura
- [ ] `META_GLOBAL_TOKEN` e `META_WABA_ID` configurados no servidor
- [ ] Canal vinculado: `external_client_id` ↔ `phone_number_id` da Meta
- [ ] Template aprovado na Meta com botões `APPT_CONFIRM` / `APPT_RESCHEDULE`
- [ ] `webhook_url` da aplicação pública (HTTPS) implementada
- [ ] Handler processa `external_client_id`, `phone_number`, `action`
- [ ] Disparo com `external_client_id` retorna `200`
- [ ] Teste de retorno (botão ou texto) chegando na sua URL
- [ ] Monitorar `/v1/usage/report` ou painel admin para volume mensal

---

## 7. Ambiente local

```bash
docker compose up -d postgres-local
# opcional: psql ... -f scripts/seed-local.sql
cd backend && go run ./cmd/api
```

Painel: `http://localhost:8080/login` — credenciais em `.env.example` (migration `000002`).

### Auditoria de segurança das dependências (antes do deploy)

Na pasta `backend`:

```bash
# 1. Limpar e validar módulos
go mod tidy
go mod verify
go list -m all          # revisar módulos; alerta se houver "replace" suspeito no go.mod

# 2. CVEs — banco oficial Go (govulncheck)
go install golang.org/x/vuln/cmd/govulncheck@latest
govulncheck ./...

# 3. Análise estática do código (gosec)
go install github.com/securego/gosec/v2/cmd/gosec@latest
gosec -exclude=G104 ./...
```

Revise saídas com atenção a vulnerabilidades na **stdlib** (atualize a versão do Go/toolchain quando indicado) e a pacotes diretos em `go.mod`.

---

## Dependências diretas atuais (`go.mod`)

| Módulo | Uso no Gateway |
|---|---|
| `github.com/golang-jwt/jwt/v5` | Sessão JWT do painel |
| `github.com/golang-migrate/migrate/v4` | Migrations SQL |
| `github.com/jackc/pgx/v5` | Driver PostgreSQL |
| `golang.org/x/crypto` | bcrypt, operações criptográficas |

Não há diretivas `replace` no `go.mod` — sinal positivo contra supply-chain hijacking via substituição de módulos.
