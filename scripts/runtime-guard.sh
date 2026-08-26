#!/bin/sh
set -eu

fail=0
reject() {
  printf 'guard: %s rejected (%s)\n' "$1" "$2" >&2
  fail=1
}

is_placeholder() {
  case "$1" in
    ""|adc|admin|password|changeme|change-me|replace-me|placeholder|example|*dev_only*|*DEV_ONLY*) return 0 ;;
  esac
  return 1
}

check_secret() {
  name=$1
  min=$2
  eval "value=\${$name-}"
  if [ -z "$value" ]; then
    reject "$name" missing
  elif is_placeholder "$value"; then
    reject "$name" default-or-placeholder
  elif [ "${#value}" -lt "$min" ]; then
    reject "$name" strength
  fi
}

case "${ADC_ENV:-dev}" in
  dev|test) exit 0 ;;
  production|prod) ;;
  *) reject ADC_ENV type ;;
esac

check_secret ADC_PG_PASSWORD 16
check_secret ADC_VALKEY_PASSWORD 16
check_secret ADC_BOOTSTRAP_ADMIN_PASSWORD 16
check_secret ADC_BOOTSTRAP_DEVICE_SECRET 32
check_secret ADC_BOOTSTRAP_APPLICATION_SECRET 32
check_secret ADC_HITL_CALLBACK_KEY 32

node_key=${ADC_NODE_KEY:-}
if [ -z "$node_key" ]; then
  reject ADC_NODE_KEY missing
elif is_placeholder "$node_key"; then
  reject ADC_NODE_KEY default-or-placeholder
elif ! printf '%s' "$node_key" | grep -Eq '^[0-9a-fA-F]{32,}$' || [ $(( ${#node_key} % 2 )) -ne 0 ]; then
  reject ADC_NODE_KEY format-or-strength
fi

for name in ADC_CE_IMAGE ADC_PY_AGENT_IMAGE ADC_CONSOLE_IMAGE; do
  eval "value=\${$name-}"
  if ! printf '%s' "$value" | grep -Eq '^[^[:space:]@]+:[^[:space:]@]+@sha256:[0-9a-f]{64}$'; then
    reject "$name" immutable-image-reference
  fi
done

kek=${ADC_DEVICE_KEK:-}
if [ -z "$kek" ]; then
  reject ADC_DEVICE_KEK missing
elif is_placeholder "$kek"; then
  reject ADC_DEVICE_KEK default-or-placeholder
elif ! printf '%s' "$kek" | grep -Eq '^[0-9a-fA-F]{64}$'; then
  reject ADC_DEVICE_KEK format-or-strength
elif [ "$kek" = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff" ]; then
  reject ADC_DEVICE_KEK known-default
fi

[ "$fail" -eq 0 ] || exit 1
printf 'guard: production configuration accepted\n'
