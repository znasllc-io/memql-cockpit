#!/bin/bash
set -euo pipefail

# run-local.sh -- connect the memQL Cockpit to a LOCAL k3d cluster without the
# dev having to remember the port-forward. Starts (or reuses) a background
# port-forward to the engine bff node -- the product-agnostic client edge every
# client connects to (platform #2472, Decision 5) -- waits for it, launches the
# Cockpit against it, and tears the forward down on exit.
#
# The port-forward is the LOCAL access config only. In staging/prod the Cockpit
# reaches the SAME bff node via the cockpit.<domain> front-door ingress
# (deploy/k8s/cockpit-front-door.yaml) -- there the equivalent is
# `memql-cockpit --cluster staging`. Same binary, config-driven endpoint.
#
# Overridable via env: MEMQL_NS, BFF_SVC, BFF_PORT, COCKPIT_BIN.

#=============================================================================
# CONFIGURATION
#=============================================================================

NAMESPACE="${MEMQL_NS:-memql}"
SERVICE="${BFF_SVC:-bff}"
PORT="${BFF_PORT:-50051}"
ENDPOINT="localhost:${PORT}"
COCKPIT_BIN="${COCKPIT_BIN:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/bin/memql-cockpit}"

PF_PID=""          # set if WE started the port-forward (so cleanup only kills ours)

#=============================================================================
# FUNCTIONS
#=============================================================================

function log()  { echo "==> $*" >&2; }
function err()  { echo "ERROR: $*" >&2; }

function check_prerequisites() {
    if ! command -v kubectl >/dev/null 2>&1; then
        err "kubectl is not installed (brew install kubectl)"; exit 4
    fi
    if [ ! -x "$COCKPIT_BIN" ]; then
        err "cockpit binary not found at $COCKPIT_BIN -- run 'make cockpit' first"; exit 4
    fi
    if ! kubectl get namespace "$NAMESPACE" >/dev/null 2>&1; then
        err "namespace '$NAMESPACE' not found -- is the local cluster up?"
        err "  run 'make up' in the memql (engine) repo first."
        exit 4
    fi
    if ! kubectl get service "$SERVICE" -n "$NAMESPACE" >/dev/null 2>&1; then
        err "no '$SERVICE' Service in namespace '$NAMESPACE'."
        err "  the engine bff node ships in the base (deploy/k8s/base/bff.yaml);"
        err "  bring the cluster up with 'make up' in the memql repo, then retry."
        exit 4
    fi
}

function port_is_open() {
    # True if something is already listening on localhost:$PORT (an existing forward).
    if command -v nc >/dev/null 2>&1; then
        nc -z localhost "$PORT" >/dev/null 2>&1
    else
        # Fallback: bash /dev/tcp probe.
        (exec 3<>"/dev/tcp/localhost/${PORT}") >/dev/null 2>&1
    fi
}

function cleanup() {
    if [ -n "$PF_PID" ] && kill -0 "$PF_PID" >/dev/null 2>&1; then
        log "stopping port-forward (pid $PF_PID)"
        kill "$PF_PID" >/dev/null 2>&1 || true
        wait "$PF_PID" 2>/dev/null || true
    fi
}

function ensure_port_forward() {
    if port_is_open; then
        log "localhost:${PORT} already open -- reusing the existing forward"
        return 0
    fi
    log "port-forwarding svc/${SERVICE} (${NAMESPACE}) -> localhost:${PORT}"
    kubectl port-forward -n "$NAMESPACE" "svc/${SERVICE}" "${PORT}:${PORT}" >/dev/null 2>&1 &
    PF_PID=$!
    trap cleanup EXIT INT TERM

    local waited=0
    until port_is_open; do
        if ! kill -0 "$PF_PID" >/dev/null 2>&1; then
            err "port-forward exited before the port opened (check 'kubectl get pods -n ${NAMESPACE}')"; exit 5
        fi
        if [ "$waited" -ge 20 ]; then
            err "timed out waiting for localhost:${PORT} to accept connections"; exit 5
        fi
        sleep 0.5; waited=$((waited + 1))
    done
    log "forward ready"
}

function launch_cockpit() {
    log "launching Cockpit against ${ENDPOINT} (press q to quit)"
    # Foreground so the TUI owns the TTY; on exit the EXIT trap stops our forward.
    "$COCKPIT_BIN" --endpoint "$ENDPOINT" "$@"
}

function main() {
    check_prerequisites
    ensure_port_forward
    launch_cockpit "$@"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
