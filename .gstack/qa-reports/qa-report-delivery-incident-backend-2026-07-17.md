# QA Report: Delivery Incident Operations Closure

| Field | Value |
|-------|-------|
| **Date** | 2026-07-17 |
| **Branch** | `main` |
| **Commit** | `11bd00f3` (the root repository currently treats `backend-go/` as untracked) |
| **Scope** | Go backend delivery-incident reporting and operations closure |
| **Environment** | Local MySQL, Redis and isolated RabbitMQ vhost |
| **Framework** | Go 1.26.1, Gin |
| **API paths** | 11 |
| **Natural closure paths** | 4 |
| **Domain MQ events** | 5 |

## Status: DONE_WITH_CONCERNS

The complete local backend technical closure passed. Real Gin routes, JWT/Redis sessions, MySQL transactions, the four fulfillment closure entry points, Outbox publication, RabbitMQ consumption, notification persistence, authorization boundaries, idempotency, concurrency and rollback behavior were exercised end to end.

This is not the final pre-production release sign-off: the external upload service did not issue the evidence token used by the test, and the PRD's 15-minute load profile over production-equivalent data was not run.

## Health Score: 96/100

| Category | Score |
|----------|-------|
| Functional API closure | 100 |
| Authorization and privacy | 100 |
| Transactional consistency | 100 |
| Events and notifications | 100 |
| Concurrency correctness | 100 |
| Performance evidence | 90 |
| Pre-production readiness | 85 |

## End-to-End Acceptance Matrix

| Area | Result | Evidence |
|------|--------|----------|
| Rider create/list/detail/evidence APIs | PASS | Real Gin routes, rider login, JWT/Redis session and MySQL writes |
| Merchant list/detail APIs | PASS | Authorized shop sees its records; another shop gets no list data and 404 on direct access |
| Admin list/detail/acknowledge/resolve/reject APIs | PASS | Exact permissions, expected-version transitions, terminal-state conflict and maker/checker fixture |
| Idempotency | PASS | Replayed request returns the same incident without duplicate main/history/audit/outbox rows |
| Evidence policy | PASS | Strict issuer/audience/subject/purpose/object-prefix/MIME/SHA/clean token checks |
| Privacy | PASS | Merchant/admin responses contain no phone, raw coordinates or object key |
| Business isolation | PASS | Incident actions do not mutate order amount, inventory, order status or delivery status |
| Pickup closure | PASS | Pickup succeeds and pickup-stage incidents resolve as `pickup_resumed` in the same transaction |
| Delivery completion closure | PASS | Completion succeeds and delivery-stage incidents resolve as `delivery_completed` |
| Force-complete closure | PASS | Request/approval flow resolves incidents as `force_completed` |
| Paid cancellation closure | PASS | All active incident types/stages resolve as `order_cancelled`; residual active count is zero |
| Natural-close failure | PASS | Injected incident update failure returns HTTP 500 and rolls back fulfillment plus incident changes |
| Five domain events | PASS | Reported, evidence-added, acknowledged, resolved and rejected Outbox events publish to RabbitMQ |
| Notification routing | PASS | Exactly the authorized merchant and admin deliveries persist; customer inbox/SMS/Push count is zero |
| 100 concurrent creates | PASS | One success, 99 active-conflict results, one active/main/history/audit/outbox record, no deadlock |
| 100 manual/natural closers | PASS | One legal terminal transition and one resolved history/outbox event |
| Write-point failures | PASS | Failures at history, audit and Outbox insert leave no partial main/item/history/audit/outbox writes |
| Migration | PASS | Isolated database completed Up -> Down -> Up; four incident tables and active-type unique index exist |
| Runtime database grants | PASS | Runtime DML exercised by integration tests; explicit DDL probe was denied |

## Performance Observation

The local non-race 100-way create burst measured P95 `211.08ms` and P99 `217.12ms`, below the PRD thresholds of P95 `300ms` and P99 `800ms`. The race-instrumented run also completed without a detected data race; its latency is not used as an SLO measurement.

## Regression Gates

| Gate | Result |
|------|--------|
| `make test-integration` | PASS |
| `make test-mq-integration` | PASS |
| `go test ./...` | PASS |
| `make test-race` | PASS |
| Delivery-incident integration tests under `-race` | PASS |
| `go vet ./...` | PASS |
| `make fmt-check` | PASS |
| `make openapi-check` | PASS |
| `make mq-contract-check` | PASS |
| `make alerts-check` | PASS, 50 rules |
| `make migrate-check` | PASS, version `202607170001` |

## Defects Found

No production-code defect was found during this acceptance run. One test-placement import cycle was found while adding the MQ E2E coverage and was resolved by moving that black-box test to the application package; no business behavior changed.

| Summary | Count |
|---------|-------|
| Production issues found | 0 |
| Verified production fixes | 0 |
| Best-effort fixes | 0 |
| Reverted fixes | 0 |
| Deferred defects | 0 |
| Release-evidence concerns | 2 |

The prior local QA baseline scored 93 and this run scores 96. The scopes differ (Customer LBS versus delivery incidents), so the `+3` is recorded for chronology and is not treated as a like-for-like product regression comparison.

## Remaining Release Evidence

1. Obtain a real evidence upload token from the pre-production upload/object-storage service and replay the create/evidence flows without the test signer.
2. Run the PRD load model for 15 minutes: create 100/300 RPS, merchant list 100 RPS, admin list 50 RPS, close 50 RPS, and pickup/complete 100 RPS over production-equivalent or million-row data.
3. Perform the planned pre-production rollout, dashboard observation and operational/security sign-off.

## Ship Readiness

| Metric | Value |
|--------|-------|
| Local backend technical closure | Ready |
| Production-code defects found | 0 |
| Active incident residuals after natural closure | 0 |
| Customer notification deliveries | 0 |
| Pre-production release sign-off | Pending external token and load evidence |

**PR Summary:** "Delivery-incident backend QA found 0 production defects; all local API, fulfillment, MQ, concurrency and rollback closures passed with a health score of 96."
