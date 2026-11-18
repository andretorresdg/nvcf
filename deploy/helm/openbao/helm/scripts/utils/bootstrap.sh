#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

openbao_status() {
    local namespace=$1 pod=$2 output rc
    output=$(kubectl exec "$pod" -n "$namespace" -c openbao -- \
        env BAO_CLIENT_TIMEOUT=5s bao status -format=json 2>/dev/null)
    rc=$?
    if [ "$rc" != 0 ] && [ "$rc" != 2 ]; then
        return 1
    fi
    printf '%s' "$output" | jq -es '
        length == 1 and (.[0] | type == "object" and
        (.initialized | type == "boolean") and
        (.sealed | type == "boolean") and
        (.initialized or .sealed))' >/dev/null 2>&1 || return 1
    printf '%s' "$output"
}

wait_for_openbao_api() {
    local namespace=$1 statefulset=$2 replicas i
    local timeout=${OPENBAO_API_TIMEOUT_SECONDS:-600}
    if ! [[ $timeout =~ ^[1-9][0-9]*$ ]]; then
        log_error "OPENBAO_API_TIMEOUT_SECONDS must be a positive integer"
        return 1
    fi
    local deadline=$((SECONDS + timeout))
    while :; do
        replicas=$(kubectl get statefulset "$statefulset" -n "$namespace" \
            -o jsonpath='{.spec.replicas}' 2>/dev/null) || replicas=""
        if [[ $replicas =~ ^[1-9][0-9]*$ ]]; then
            break
        fi
        if [ "$SECONDS" -ge "$deadline" ]; then
            log_error "Timed out waiting for StatefulSet '$statefulset'"
            return 1
        fi
        sleep 2
    done
    OPENBAO_REPLICAS=$replicas
    for ((i = 0; i < replicas; i++)); do
        until openbao_status "$namespace" "$statefulset-$i" >/dev/null; do
            if [ "$SECONDS" -ge "$deadline" ]; then
                log_error "Timed out waiting for OpenBao API in '$statefulset-$i'"
                return 1
            fi
            log_info "Waiting for OpenBao API in '$statefulset-$i'"
            sleep 2
        done
    done
}

# BOOTSTRAP_INITIALIZED is read by the deployment script.
# shellcheck disable=SC2034
check_bootstrap_credentials() {
    local namespace=$1 statefulset=$2 mode=$3 status root key existing
    BOOTSTRAP_INITIALIZED=false
    status=$(openbao_status "$namespace" "$statefulset-0") || return 1
    if [ "$(printf '%s' "$status" | jq -r '.initialized')" = true ]; then
        root=$(get_root_token "$namespace" "$statefulset") || return 1
        if [ -z "$root" ] || [ "$root" = null ]; then
            log_error "OpenBao is initialized but the saved root token is missing; restore bootstrap credentials"
            return 1
        fi
        if [ "$mode" = shamir ]; then
            key=$(get_unseal_key "$namespace" "$statefulset") || return 1
            if [ -z "$key" ] || [ "$key" = null ]; then
                log_error "OpenBao is initialized but the saved unseal key is missing; restore bootstrap credentials"
                return 1
            fi
        fi
        BOOTSTRAP_INITIALIZED=true
        log_info "OpenBao is initialized; reusing saved bootstrap credentials"
        return 0
    fi
    local suffix
    for suffix in root-token recovery-keys; do
        existing=$(kubectl get secret "$statefulset-$suffix" \
            -n "$namespace" --ignore-not-found -o name) || return 1
        if [ -n "$existing" ]; then
            log_error "OpenBao is uninitialized but bootstrap credentials exist; restore the matching data"
            return 1
        fi
    done
    if [ "$mode" = shamir ]; then
        key=$(get_unseal_key "$namespace" "$statefulset") || return 1
        if [ -n "$key" ]; then
            log_error "OpenBao is uninitialized but an unseal key exists; restore the matching data"
            return 1
        fi
    fi
}

unseal_openbao_members() {
    local namespace=$1 statefulset=$2 mode=$3 i status key deadline
    wait_for_openbao_api "$namespace" "$statefulset" || return 1
    if [ "$mode" = shamir ]; then
        key=$(get_unseal_key "$namespace" "$statefulset") || return 1
        if [ -z "$key" ] || [ "$key" = null ]; then
            log_error "Cannot unseal OpenBao without the saved unseal key"
            return 1
        fi
        for ((i = 0; i < OPENBAO_REPLICAS; i++)); do
            status=$(openbao_status "$namespace" "$statefulset-$i") || return 1
            if [ "$(printf '%s' "$status" | jq -r '.sealed')" = false ]; then
                continue
            fi
            if [ "$(printf '%s' "$status" | jq -r '.initialized')" = false ]; then
                if [ "$i" = 0 ]; then
                    log_error "The primary member is not initialized"
                    return 1
                fi
                kubectl exec "$statefulset-$i" -c openbao -n "$namespace" -- \
                    bao operator raft join "http://$statefulset-0.$statefulset-internal:8200" || return 1
            fi
            kubectl exec "$statefulset-$i" -c openbao -n "$namespace" -- \
                bao operator unseal "$key" || return 1
        done
    fi
    deadline=$((SECONDS + ${OPENBAO_API_TIMEOUT_SECONDS:-600}))
    for ((i = 0; i < OPENBAO_REPLICAS; i++)); do
        while :; do
            status=$(openbao_status "$namespace" "$statefulset-$i") || status=""
            if [ -n "$status" ] && [ "$(printf '%s' "$status" | jq -r '.sealed')" = false ]; then
                break
            fi
            if [ "$SECONDS" -ge "$deadline" ]; then
                log_error "Timed out waiting for '$statefulset-$i' to unseal"
                return 1
            fi
            sleep 2
        done
    done
    log_success "All OpenBao members are initialized and unsealed"
}

# The post-install hook must not race the bootstrap Job, even without Helm --wait.
wait_for_openbao_bootstrap() {
    local namespace=$1 statefulset=$2 i status complete deadline
    wait_for_openbao_api "$namespace" "$statefulset" || return 1
    deadline=$((SECONDS + ${OPENBAO_API_TIMEOUT_SECONDS:-600}))
    while :; do
        complete=true
        for ((i = 0; i < OPENBAO_REPLICAS; i++)); do
            status=$(openbao_status "$namespace" "$statefulset-$i") || status=""
            if [ -z "$status" ] || ! printf '%s' "$status" | jq -e '.initialized and (.sealed | not)' >/dev/null; then
                complete=false
                break
            fi
        done
        if [ "$complete" = true ]; then
            return 0
        fi
        if [ "$SECONDS" -ge "$deadline" ]; then
            log_error "Timed out waiting for OpenBao bootstrap to finish"
            return 1
        fi
        sleep 2
    done
}
