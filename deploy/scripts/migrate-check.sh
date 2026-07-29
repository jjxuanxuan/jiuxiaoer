#!/usr/bin/env bash
set -euo pipefail

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
	if [[ "${ALLOW_EXTERNAL_SKIP:-0}" == "1" && "${CI:-false}" != "true" ]]; then
		echo "SKIP migrate-check: a running Docker daemon is not available"
		exit 0
	fi
	echo "FAIL migrate-check: a running Docker daemon is required for disposable MySQL 8.4" >&2
	exit 2
fi

container="jxe-migrate-check-${PPID}-$$"
database="jxe_migrate_check"
roundtrip_database="jxe_migrate_roundtrip"
root_password="jxe-migrate-check-root"
goose_version="v3.26.0"
checkpoint_dir="$(mktemp -d "${TMPDIR:-/tmp}/jxe-migrate-check.XXXXXX")"

cleanup() {
	docker rm -f "${container}" >/dev/null 2>&1 || true
	rm -f \
		"${checkpoint_dir}/payments.json" \
		"${checkpoint_dir}/refunds.json" \
		"${checkpoint_dir}/returns.json"
	rmdir "${checkpoint_dir}" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

echo "migrate-check: starting disposable MySQL 8.4"
docker run --detach \
	--name "${container}" \
	--env "MYSQL_ROOT_PASSWORD=${root_password}" \
	--env "MYSQL_DATABASE=${database}" \
	--env "TZ=Asia/Shanghai" \
	--publish "127.0.0.1::3306" \
	--health-cmd="mysqladmin ping -h 127.0.0.1 -uroot -p${root_password}" \
	--health-interval=2s \
	--health-timeout=2s \
	--health-retries=40 \
	mysql:8.4 \
	--character-set-server=utf8mb4 \
	--collation-server=utf8mb4_0900_ai_ci \
	--default-time-zone=+08:00 >/dev/null

for _ in $(seq 1 60); do
	status="$(docker inspect --format '{{.State.Health.Status}}' "${container}" 2>/dev/null || true)"
	[[ "${status}" == "healthy" ]] && break
	sleep 1
done
if [[ "${status:-}" != "healthy" ]]; then
	docker logs "${container}" >&2 || true
	echo "FAIL migrate-check: disposable MySQL did not become healthy" >&2
	exit 1
fi

port="$(docker port "${container}" 3306/tcp | awk -F: 'NR==1 {print $NF}')"
if [[ -z "${port}" ]]; then
	echo "FAIL migrate-check: cannot resolve disposable MySQL port" >&2
	exit 1
fi

mysql_root=(docker exec -i "${container}" mysql -uroot "-p${root_password}" --batch --skip-column-names)
server_facts="$("${mysql_root[@]}" -e "SELECT VERSION(), @@global.time_zone, @@session.time_zone")"
if [[ "${server_facts}" != 8.4.*$'\t+08:00\t+08:00' ]]; then
	echo "FAIL migrate-check: expected MySQL 8.4 and +08:00 global/session timezone, got ${server_facts}" >&2
	exit 1
fi

dsn_for() {
	local schema="$1"
	printf 'root:%s@tcp(127.0.0.1:%s)/%s?charset=utf8mb4&parseTime=true&loc=Local&time_zone=%%27%%2B08%%3A00%%27' \
		"${root_password}" "${port}" "${schema}"
}

goose() {
	local directory="$1"
	local dsn="$2"
	shift 2
	go run "github.com/pressly/goose/v3/cmd/goose@${goose_version}" \
		-no-color -dir "${directory}" mysql "${dsn}" "$@"
}

manual_goose_up() {
	local dsn="$1"
	go run "github.com/pressly/goose/v3/cmd/goose@${goose_version}" \
		-no-color -allow-missing -dir ./migrations/manual mysql "${dsn}" up
}

primary_dsn="$(dsn_for "${database}")"
echo "migrate-check: applying normal migrations"
goose ./migrations "${primary_dsn}" up

echo "migrate-check: simulating old writers and return lifecycle changes"
"${mysql_root[@]}" "${database}" <<'SQL'
INSERT INTO orders
  (id, order_no, customer_id, merchant_id, shop_id, status, pay_status, delivery_status)
VALUES
  (810001, 'MIGRATE-ORDER-1', 820001, 830001, 840001, 'paid', 'succeeded', 'pending');

INSERT INTO payments
  (id, payment_no, order_id, customer_id, channel, provider, status, amount, currency)
VALUES
  (850001, 'MIGRATE-PAYMENT-1', 810001, 820001, 'wechat', 'wechat', 'succeeded', 100, 'CNY');

INSERT INTO after_sales
  (id, after_sale_no, order_id, customer_id, merchant_id, shop_id, type,
   requested_resolution, approved_resolution, status, requested_amount,
   approved_amount, description, submitted_at)
VALUES
  (860001, 'MIGRATE-AFTER-SALE-1', 810001, 820001, 830001, 840001, 'damaged',
   'refund_only', 'refund_only', 'refund_processing', 100, 100,
   'migrate-check fixture', CURRENT_TIMESTAMP(3)),
  (860002, 'MIGRATE-AFTER-SALE-2', 810001, 820001, 830001, 840001, 'damaged',
   'refund_only', 'refund_only', 'refund_processing', 100, 100,
   'migrate-check fixture', CURRENT_TIMESTAMP(3)),
  (860003, 'MIGRATE-AFTER-SALE-3', 810001, 820001, 830001, 840001, 'damaged',
   'refund_only', 'refund_only', 'refund_processing', 100, 100,
   'migrate-check fixture', CURRENT_TIMESTAMP(3));

INSERT INTO refunds
  (id, refund_no, after_sale_id, order_id, payment_id, provider, status,
   amount, total_amount, currency, requested_at)
VALUES
  (870001, 'MIGRATE-REFUND-1', 860001, 810001, 850001, 'wechat', 'processing',
   100, 100, 'CNY', CURRENT_TIMESTAMP(3));

INSERT INTO delivery_returns
  (id, return_no, delivery_order_id, active_delivery_order_id, order_id,
   shop_id, rider_id, reason_code, status, initiator_type, initiator_id,
   requested_at)
VALUES
  (880001, 'MIGRATE-RETURN-REQUESTED', 890001, 890001, 810001,
   840001, 900001, 'other', 'requested', 'rider', 900001, CURRENT_TIMESTAMP(3)),
  (880002, 'MIGRATE-RETURN-RETURNING', 890002, 890002, 810001,
   840001, 900001, 'other', 'requested', 'rider', 900001, CURRENT_TIMESTAMP(3)),
  (880003, 'MIGRATE-RETURN-CLOSED', 890003, 890003, 810001,
   840001, 900001, 'other', 'requested', 'rider', 900001, CURRENT_TIMESTAMP(3));

UPDATE delivery_returns
   SET status='returning', after_sale_id=860002
 WHERE id=880002;
UPDATE delivery_returns
   SET status='returning', after_sale_id=860003
 WHERE id=880003;
UPDATE delivery_returns
   SET status='closed', active_delivery_order_id=NULL,
       closed_at=CURRENT_TIMESTAMP(3)
 WHERE id=880003;
SQL

run_backfill() {
	local job="$1"
	local checkpoint="$2"
	JXE_MYSQL_DSN="${primary_dsn}" \
	JXE_MYSQL_REQUIRED=true \
	JXE_MYSQL_REQUIRED_TIME_ZONE="+08:00" \
	JXE_WINE_TICKET_BACKFILL_ALLOW_WRITE=true \
	TZ=Asia/Shanghai \
	go run ./cmd/wine-ticket-backfill \
		--job "${job}" \
		--execute \
		--confirm APPLY_WINE_TICKET_REGISTRY_BACKFILL \
		--checkpoint "${checkpoint}" \
		--batch-size 500 \
		--rows-per-second 10000 >/dev/null
}

echo "migrate-check: exercising resumable registry backfills"
run_backfill wine-ticket-payments "${checkpoint_dir}/payments.json"
run_backfill wine-ticket-refunds "${checkpoint_dir}/refunds.json"
run_backfill wine-ticket-returns "${checkpoint_dir}/returns.json"

assertions="$("${mysql_root[@]}" "${database}" -e "
SELECT
  (SELECT COUNT(*) FROM payments
    WHERE biz_type <> 'retail_order' OR biz_id <> order_id),
  (SELECT COUNT(*) FROM refunds
    WHERE biz_type <> 'retail_after_sale' OR biz_id <> after_sale_id),
  (SELECT COUNT(*) FROM delivery_returns
    WHERE (id=880001 AND NOT (settlement_type='retail_cash_refund' AND settlement_status='not_started' AND settlement_biz_id IS NULL))
       OR (id=880002 AND NOT (settlement_type='retail_cash_refund' AND settlement_status='processing' AND settlement_biz_id=860002))
       OR (id=880003 AND NOT (settlement_type='retail_cash_refund' AND settlement_status='succeeded' AND settlement_biz_id=860003 AND settled_at IS NOT NULL)));
")"
if [[ "${assertions}" != $'0\t0\t0' ]]; then
	echo "FAIL migrate-check: backfill assertions returned ${assertions}" >&2
	exit 1
fi

echo "migrate-check: applying Contract after backfill assertions"
manual_goose_up "${primary_dsn}"

contract_facts="$("${mysql_root[@]}" "${database}" -e "
SELECT
  SUM(COLUMN_NAME IN ('biz_type','biz_id') AND IS_NULLABLE <> 'NO')
FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA='${database}'
  AND TABLE_NAME IN ('payments','refunds')
  AND COLUMN_NAME IN ('biz_type','biz_id');
SELECT COUNT(*)
FROM information_schema.TABLE_CONSTRAINTS
WHERE CONSTRAINT_SCHEMA='${database}'
  AND CONSTRAINT_NAME IN (
    'chk_payment_business_link',
    'chk_refund_business_link',
    'chk_delivery_return_settlement_type',
    'chk_delivery_return_settlement_state'
  )
  AND CONSTRAINT_TYPE='CHECK';
")"
if [[ "${contract_facts}" != $'0\n4' ]]; then
	echo "FAIL migrate-check: Contract schema assertions returned ${contract_facts}" >&2
	exit 1
fi

echo "migrate-check: running empty database up -> Contract down -> Expand down -> up"
"${mysql_root[@]}" -e \
	"CREATE DATABASE \`${roundtrip_database}\` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"
roundtrip_dsn="$(dsn_for "${roundtrip_database}")"
goose ./migrations "${roundtrip_dsn}" up-to 202607270005

echo "migrate-check: exercising single-operator RBAC and history migration"
"${mysql_root[@]}" "${roundtrip_database}" <<'SQL'
INSERT INTO roles (id, code, name, scope, status) VALUES
  (1001, 'super_admin', '超级管理员', 'all', 'active'),
  (1002, 'admin_manager', '管理员', 'all', 'active'),
  (1003, 'operation', '运营', 'scoped', 'active'),
  (1004, 'finance', '财务', 'readonly', 'active');

INSERT INTO accounts (
  id, account_type, username, status, credential_version
) VALUES (
  990010, 'admin', 'migrate_check_single_operator', 'active', 7
);

INSERT INTO admin_users (
  id, account_id, role_id, admin_sub_role, name, status
) VALUES (
  990003, 990010, 1003, 'operation', '迁移验收管理员', 'active'
);

INSERT INTO permissions (id, code, resource, action, description, status) VALUES
  (2048, 'asset_adjustment:create', 'asset_adjustment', 'create', '创建资产调账', 'active'),
  (2049, 'asset_adjustment:approve', 'asset_adjustment', 'approve', '审批资产调账', 'active'),
  (2068, 'delivery:force_complete', 'delivery', 'force_complete', '强制完成配送', 'active');

INSERT INTO role_permissions (id, role_id, permission_id)
SELECT r.id * 1000 + p.id, r.id, p.id
FROM roles r
JOIN permissions p ON (
  (p.code='asset_adjustment:approve' AND r.code IN ('super_admin','admin_manager','finance'))
  OR (p.code='delivery:force_complete_request' AND r.code IN ('super_admin','operation'))
  OR (p.code='delivery:force_complete_approve' AND r.code IN ('super_admin','admin_manager'))
  OR (p.code='wine_ticket_exception:review' AND r.code IN ('super_admin','admin_manager','operation'))
);

INSERT INTO admin_override_approvals (
  id, action, resource_type, resource_id, maker_admin_id, checker_admin_id,
  reason_code, reason, expected_version, status, expires_at, request_id
) VALUES (
  990001, 'delivery.force_complete', 'delivery_order', 990002, 990003, 990004,
  'MIGRATE_CHECK', 'preserve approval history', 7, 'pending',
  CURRENT_TIMESTAMP(3) + INTERVAL 1 HOUR, 'migrate-check-single-operator'
);
SQL

goose ./migrations "${roundtrip_dsn}" up

single_operator_facts="$("${mysql_root[@]}" "${roundtrip_database}" -e "
SELECT COUNT(*)
FROM permissions
WHERE code IN (
  'asset_adjustment:approve',
  'delivery:force_complete_request',
  'delivery:force_complete_approve',
  'wine_ticket_exception:review'
) AND status <> 'inactive';
SELECT COUNT(*)
FROM role_permissions rp
JOIN permissions p ON p.id=rp.permission_id
WHERE p.code IN (
  'asset_adjustment:approve',
  'delivery:force_complete_request',
  'delivery:force_complete_approve',
  'wine_ticket_exception:review'
) AND rp.deleted_at IS NULL;
SELECT COUNT(*)
FROM role_permissions rp
JOIN roles r ON r.id=rp.role_id
JOIN permissions p ON p.id=rp.permission_id
WHERE rp.deleted_at IS NULL
  AND (
    (p.code='asset_adjustment:create' AND r.code IN ('super_admin','admin_manager','finance'))
    OR (p.code='delivery:force_complete' AND r.code IN ('super_admin','admin_manager','operation'))
    OR (p.code='wine_ticket_exception:resolve' AND r.code IN ('super_admin','admin_manager','operation'))
    OR (p.code='wine_ticket_package:publish' AND r.code IN ('super_admin','admin_manager','operation'))
  );
SELECT COUNT(*)
FROM admin_override_approvals
WHERE id=990001 AND status='expired'
  AND maker_admin_id=990003 AND checker_admin_id=990004
  AND reason='preserve approval history'
  AND request_id='migrate-check-single-operator';
SELECT COUNT(*)
FROM accounts
WHERE id=990010 AND credential_version=8
  AND token_invalid_before IS NOT NULL;
")"
if [[ "${single_operator_facts}" != $'0\n0\n12\n1\n1' ]]; then
	echo "FAIL migrate-check: single-operator Up assertions returned ${single_operator_facts}" >&2
	exit 1
fi

manual_goose_up "${roundtrip_dsn}"
goose ./migrations/manual "${roundtrip_dsn}" down
goose ./migrations "${roundtrip_dsn}" down

single_operator_down_facts="$("${mysql_root[@]}" "${roundtrip_database}" -e "
SELECT COUNT(*)
FROM permissions
WHERE code IN (
  'asset_adjustment:approve',
  'delivery:force_complete_request',
  'delivery:force_complete_approve',
  'wine_ticket_exception:review'
) AND status='active';
SELECT COUNT(*)
FROM role_permissions rp
JOIN permissions p ON p.id=rp.permission_id
WHERE p.code IN (
  'asset_adjustment:approve',
  'delivery:force_complete_request',
  'delivery:force_complete_approve',
  'wine_ticket_exception:review'
) AND rp.deleted_at IS NULL;
SELECT COUNT(*)
FROM role_permissions rp
JOIN roles r ON r.id=rp.role_id
JOIN permissions p ON p.id=rp.permission_id
WHERE p.code='delivery:force_complete'
  AND r.code IN ('admin_manager','operation')
  AND rp.deleted_at IS NULL;
SELECT COUNT(*)
FROM role_permissions rp
JOIN roles r ON r.id=rp.role_id
JOIN permissions p ON p.id=rp.permission_id
WHERE p.code='delivery:force_complete'
  AND r.code='super_admin'
  AND rp.deleted_at IS NULL;
SELECT COUNT(*)
FROM admin_override_approvals
WHERE id=990001 AND status='expired'
  AND maker_admin_id=990003 AND checker_admin_id=990004;
SELECT COUNT(*)
FROM accounts
WHERE id=990010 AND credential_version=9
  AND token_invalid_before IS NOT NULL;
")"
if [[ "${single_operator_down_facts}" != $'4\n10\n0\n1\n1\n1' ]]; then
	echo "FAIL migrate-check: single-operator Down assertions returned ${single_operator_down_facts}" >&2
	exit 1
fi

# 历史迁移有意包含不可逆的证据表。
# 按酒票运行手册要求，只回滚本功能的常规迁移：
# 先回滚 Contract，再回滚酒票 Expand 系列。
goose ./migrations "${roundtrip_dsn}" down-to 202607220006
goose ./migrations "${roundtrip_dsn}" up
manual_goose_up "${roundtrip_dsn}"

echo "migrate-check: PASS"
