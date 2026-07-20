#!/usr/bin/env bash
set -euo pipefail

container="${JXE_MYSQL_CONTAINER:-jxe-p0-mysql}"
database="${JXE_MYSQL_DATABASE:-jiuxiaoer_go_p0}"
runtime_user="${JXE_MYSQL_RUNTIME_USER:-jxe_app}"
runtime_password="${JXE_MYSQL_RUNTIME_PASSWORD:-p0apppass}"
root_password="${JXE_MYSQL_ROOT_PASSWORD:-rootpass}"

mysql_root=(docker exec -i "${container}" mysql -uroot "-p${root_password}" --batch --skip-column-names)

"${mysql_root[@]}" <<SQL
CREATE USER IF NOT EXISTS '${runtime_user}'@'%' IDENTIFIED BY '${runtime_password}';
ALTER USER '${runtime_user}'@'%' IDENTIFIED BY '${runtime_password}';
REVOKE ALL PRIVILEGES, GRANT OPTION FROM '${runtime_user}'@'%';
SQL

tables=$("${mysql_root[@]}" -e "SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA='${database}' AND TABLE_TYPE='BASE TABLE' AND TABLE_NAME NOT IN ('asset_entries','delivery_incidents','delivery_incident_items','delivery_incident_evidence','delivery_incident_history','delivery_returns','delivery_return_history','return_receipt_items','wechat_bill_reconciliation_runs','wechat_bill_observations','wechat_bill_discrepancies')")
grant_sql=""
while IFS= read -r table; do
	[[ -z "${table}" ]] && continue
	printf -v statement 'GRANT SELECT, INSERT, UPDATE, DELETE ON `%s`.`%s` TO '\''%s'\''@'\''%%'\'';' "${database}" "${table}" "${runtime_user}"
	grant_sql+="${statement}"$'\n'
done <<<"${tables}"
if [[ -n "${grant_sql}" ]]; then
	printf '%s' "${grant_sql}" | "${mysql_root[@]}"
fi

"${mysql_root[@]}" <<SQL
GRANT SELECT, INSERT ON \`${database}\`.\`asset_entries\` TO '${runtime_user}'@'%';
GRANT SELECT, INSERT, UPDATE ON \`${database}\`.\`delivery_incidents\` TO '${runtime_user}'@'%';
GRANT SELECT, INSERT ON \`${database}\`.\`delivery_incident_items\` TO '${runtime_user}'@'%';
GRANT SELECT, INSERT ON \`${database}\`.\`delivery_incident_evidence\` TO '${runtime_user}'@'%';
GRANT SELECT, INSERT ON \`${database}\`.\`delivery_incident_history\` TO '${runtime_user}'@'%';
GRANT SELECT, INSERT, UPDATE ON \`${database}\`.\`delivery_returns\` TO '${runtime_user}'@'%';
GRANT SELECT, INSERT ON \`${database}\`.\`delivery_return_history\` TO '${runtime_user}'@'%';
GRANT SELECT, INSERT ON \`${database}\`.\`return_receipt_items\` TO '${runtime_user}'@'%';
GRANT SELECT, INSERT, UPDATE ON \`${database}\`.\`wechat_bill_reconciliation_runs\` TO '${runtime_user}'@'%';
GRANT SELECT, INSERT, DELETE ON \`${database}\`.\`wechat_bill_observations\` TO '${runtime_user}'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON \`${database}\`.\`wechat_bill_discrepancies\` TO '${runtime_user}'@'%';
FLUSH PRIVILEGES;
SQL

echo "provisioned ${runtime_user} with least-privilege asset, incident, delivery-return, and reconciliation permissions"
