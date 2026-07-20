# Jiuxiaoer Go Backend

The Go modular monolith is the source of truth for the phase-one transaction and fulfilment flow. CP1 adds reliable print tasks, transaction notifications and inbox, pickup/delivery verification codes, account provisioning, controlled order recovery, and the server-side real-name/adult gate for restricted products.

## Local Run

```bash
cp .env.example .env.local
make deps-up
make migrate-up
make seed
make run
```

The Go backend must use its own database (the local default is `jiuxiaoer_go_p0`). The legacy Node backend runs Sequelize schema synchronization at startup and must never point at the same database. Startup verifies migration sentinel columns and fails with an actionable error when the schema is incompatible.

Endpoints:

- `GET /livez`: process liveness only.
- `GET /readyz`: required dependency and Snowflake lease readiness.
- `GET /api/v1/health`: compatibility readiness endpoint using the API envelope.
- `GET /metrics`: Prometheus text metrics; production requires `Authorization: Bearer $JXE_METRICS_TOKEN`.
- `GET /api/v1/swagger/index.html`: API documentation.
- `POST /api/v1/auth/customer/send-code`: send a customer login code through the configured SMS provider.
- `POST /api/v1/auth/customer/sms-login`: exchange a phone number and one-time code for a customer session.
- `POST /api/v1/auth/customer/wechat-login`: exchange a Mini Program code for a customer session.
- `POST /api/v1/auth/customer/phone-bind`: bind the authenticated customer's WeChat-authorized phone.
- `POST /api/v1/orders/:id/payments`: idempotently create a provider payment.
- `GET /api/v1/orders/:id/payment`: read the persisted payment state.
- `POST /api/v1/payments/:provider/callbacks`: receive provider-signed callbacks.
- `POST /api/v1/identity-verifications`: create an idempotent provider-hosted identity/adult-verification session; the client never submits a name or document number to this API.
- `GET /api/v1/identity-verifications/:id`: poll one customer-owned verification request.
- `POST /api/v1/identity-verifications/:provider/callbacks`: verify a provider callback and actively query the authoritative result before updating the adult-authorization fact.

## Multi-instance Requirements

Every live instance needs a stable `JXE_INSTANCE_ID` and a Snowflake node ID in `0..1023`. Redis holds a renewable lease for each node ID; a second instance using the same ID fails startup. Losing the lease makes the instance unready and then terminates it before the lease can expire.

Outbox workers claim rows using `locked_by` and `locked_until` under `FOR UPDATE SKIP LOCKED`. Delivery remains at-least-once, so consumers must continue deduplicating by `event_id`.

## Production Guardrails

`JXE_APP_ENV=production` rejects startup when required MySQL/Redis/RabbitMQ configuration is absent, JWT secrets are default/short/shared, metrics lacks a bearer token, any mock provider is enabled, Tencent Cloud SMS credentials/sign/template are incomplete, or WeChat identity/payment credentials are incomplete. The Tencent Cloud SecretID/SecretKey, Mini Program secret, merchant private key, API v3 key, provider credentials, and JWT secrets must come from the deployment secret manager, never `system_configs` or Git.

Production SMS uses Tencent Cloud SMS API 3.0. Configure `JXE_SMS_MOCK_ENABLED=false` and the `JXE_SMS_TENCENTCLOUD_*` settings from `.env.example`. The approved verification template must have two variables in this exact order: the six-digit code and its validity in minutes, for example `您的验证码为{1}，{2}分钟内有效。`.

## Verification

```bash
make verify
make migrate-check
make test-integration
# Complete local phase-one gate (verify + migration + seed + integration + alerts)
make verify-cp1
```

L2 服务区、首页与订单强校验运行手册：`docs/runbooks/l2-service-area-home.md`。

Integration tests require the local Compose dependencies and applied migrations. The suite covers the P0 transaction chain, identity/phone binding, payment creation and callbacks, provider query reconciliation, unpaid expiry, 1000-way callback/expiry contention, inventory contention, Snowflake lease collision, concurrent Outbox claims, and the CP1 compliance/print/provisioning/assignment/verification/notification/account-revocation chain including provider-hosted identity sessions, signed/query-confirmed callbacks, duplicate callback collapse, identity revocation, and genuine two-principal force completion.

CP1 capabilities are additive and external side effects default off. Use the
`JXE_CP1_*` settings in `.env.example`; production rejects fake providers and
default security secrets. Operational recovery and rollback are documented in
`docs/runbooks/cp1-launch-closure.md`. The generated OpenAPI endpoint is the only
client contract and includes every registered `/api/v1` route.
