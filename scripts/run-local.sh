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
# Overridable via env: MEMQL_NS, BFF_SVC, BFF_PORT, COCKPIT_BIN,
# MEMQL_ALLOW_CONTEXT (=1 to skip the local-context guard).

#=============================================================================
# CONFIGURATION
#=============================================================================

NAMESPACE="${MEMQL_NS:-memql}"
SERVICE="${BFF_SVC:-bff}"
PORT="${BFF_PORT:-50051}"
ENDPOINT="localhost:${PORT}"
COCKPIT_BIN="${COCKPIT_BIN:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/bin/memql-cockpit}"
# The Cockpit's kube-context must be a LOCAL k3d cluster: staging/prod use the
# SAME 'memql' namespace + 'bff' Service, so without this guard `make run-local`
# would silently port-forward to a remote cluster. Set MEMQL_ALLOW_CONTEXT=1 to
# override (e.g. a differently-named local cluster).
LOCAL_CONTEXT_PREFIX="k3d-"

PF_PID=""          # set only if WE start the port-forward (so cleanup kills only ours)
PF_ERR=""          # temp file capturing the forward's stderr for diagnostics

#=============================================================================
# FUNCTIONS
#=============================================================================

function log() { echo "==> $*" >&2; }
function err() { echo "ERROR: $*" >&2; }

function cleanup() {
    if [ -n "$PF_PID" ] && kill -0 "$PF_PID" >/dev/null 2>&1; then
        log "stopping port-forward (pid $PF_PID)"
        kill "$PF_PID" >/dev/null 2>&1 || true
        wait "$PF_PID" 2>/dev/null || true
    fi
    PF_PID=""
    [ -n "$PF_ERR" ] && rm -f "$PF_ERR" && PF_ERR=""
    return 0
}

function check_prerequisites() {
    if ! command -v kubectl >/dev/null 2>&1; then
        err "kubectl is not installed (brew install kubectl)"; exit 4
    fi
    if [ ! -x "$COCKPIT_BIN" ]; then
        err "cockpit binary not found at $COCKPIT_BIN -- run 'make cockpit' first"; exit 4
    fi

    local ctx; ctx="$(kubectl config current-context 2>/dev/null || echo '<none>')"
    log "kube-context: ${ctx}"
    if [ "${MEMQL_ALLOW_CONTEXT:-0}" != "1" ] && [ "${ctx#"$LOCAL_CONTEXT_PREFIX"}" = "$ctx" ]; then
        err "context '${ctx}' is not a local k3d cluster (expected '${LOCAL_CONTEXT_PREFIX}*')."
        err "  staging/prod share the same '${NAMESPACE}' namespace + '${SERVICE}' Service, so"
        err "  this would forward to a REMOTE cluster. Switch context (kubectl config use-context k3d-...),"
        err "  or set MEMQL_ALLOW_CONTEXT=1 to override."
        exit 3
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
    # True if something is already listening on localhost:$PORT.
    if command -v nc >/dev/null 2>&1; then
        nc -z localhost "$PORT" >/dev/null 2>&1
    else
        # fd 3 is scoped to this subshell, so it closes when the subshell exits.
        (exec 3<>"/dev/tcp/localhost/${PORT}") >/dev/null 2>&1
    fi
}

function ensure_port_forward() {
    if port_is_open; then
        log "something is already listening on localhost:${PORT} -- assuming it is the ${SERVICE} forward, reusing it"
        return 0
    fi

    PF_ERR="$(mktemp)"
    trap 'cleanup' EXIT
    trap 'cleanup; exit 130' INT
    trap 'cleanup; exit 143' TERM

    log "port-forwarding svc/${SERVICE} (${NAMESPACE}) -> localhost:${PORT}"
    kubectl port-forward -n "$NAMESPACE" "svc/${SERVICE}" "${PORT}:${PORT}" >/dev/null 2>"$PF_ERR" &
    PF_PID=$!

    local waited=0
    until port_is_open; do
        if ! kill -0 "$PF_PID" >/dev/null 2>&1; then
            err "port-forward exited before the port opened:"
            sed 's/^/    /' "$PF_ERR" >&2 2>/dev/null || true
            err "  (check 'kubectl get pods -n ${NAMESPACE}')"
            exit 5
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
