# L2 Service Area and Home Operations

## Resolver timeout

1. Check `jxe_service_area_resolve_total{result="timeout"}` and HTTP 504 volume.
2. Inspect MySQL latency and the `shops` / `shop_business_hours` query plan.
3. Confirm city candidate count remains below 500 and `idx_shop_service_city` is used.
4. Keep order enforcement enabled. Do not bypass service-area checks to reduce errors.
5. Roll back the API release if timeouts persist after MySQL recovery.

## Unavailable spike

1. Split metrics by `result`; compare `CITY_UNSUPPORTED`, `NO_OPEN_SHOP`, and `OUT_OF_SERVICE_AREA`.
2. Verify the affected city's shop status, weekly hours, radius, coordinates, and city code.
3. Check `service_city_version:{city_code}` in Redis after a merchant status change.
4. Confirm location parameters are valid before changing service radius configuration.

## Home content stale or missing

1. Check the slot is `published` and inside its `start_at` / `end_at` window.
2. Verify referenced products and categories remain active.
3. Check `home_version:{city_code}` and `home_version:global` in Redis.
4. Inspect pending/dead `home_slot.changed` and `cache.invalidate` Outbox events.

## Rollback

- Set `JXE_ORDER_SERVICE_AREA_ENFORCEMENT=observe` only during an approved emergency rollback.
- Roll back application code before running the L2 down migration.
- Do not drop L2 columns while any application instance writes delivery promise snapshots.
