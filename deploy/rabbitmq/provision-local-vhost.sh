#!/usr/bin/env bash
set -euo pipefail

container="${JXE_RABBITMQ_CONTAINER:-jxe-p0-rabbitmq}"
vhost="${1:-jxe-events-v2}"
user="${2:-jxe}"

if ! docker inspect "$container" >/dev/null 2>&1; then
  echo "RabbitMQ container not found: $container" >&2
  exit 1
fi

if ! docker exec "$container" rabbitmqctl list_vhosts --silent | grep -Fxq "$vhost"; then
  docker exec "$container" rabbitmqctl add_vhost "$vhost"
fi

docker exec "$container" rabbitmqctl set_permissions -p "$vhost" "$user" '.*' '.*' '.*'
echo "RabbitMQ vhost ready: $vhost (user: $user)"
