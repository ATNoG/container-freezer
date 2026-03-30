#!/usr/bin/env bash
# Benchmark: Cold Start vs CRIU Checkpoint/Restore (full Knative integration)
# Run from a machine with kubectl access to the cluster.
set -euo pipefail

ITERATIONS=${1:-50}
NAMESPACE="default"
KN_NS="knative-serving"
SERVICE_LABEL="serving.knative.dev/service=retransmitter"
SETTLE_TIME=15  # seconds between tests for pod to receive events
HELPER_POD="bench-helper"
OUTPUT="criu_benchmark_$(date +%Y%m%d_%H%M%S).csv"

echo "=== CRIU Integration Benchmark: $ITERATIONS iterations ==="
echo "iteration,cold_start_ms,freeze_ms,restore_ms" > "$OUTPUT"

cleanup() {
    echo "Cleaning up helper pod..."
    kubectl delete pod $HELPER_POD -n $NAMESPACE --ignore-not-found 2>/dev/null || true
}
trap cleanup EXIT

# ── Helper pod (persistent, for in-cluster HTTP requests) ──────────────
echo "Setting up in-cluster helper pod..."
kubectl delete pod $HELPER_POD -n $NAMESPACE --ignore-not-found 2>/dev/null || true
sleep 2
kubectl run $HELPER_POD -n $NAMESPACE --image=curlimages/curl --restart=Never -- sleep 86400
kubectl wait --for=condition=Ready pod/$HELPER_POD -n $NAMESPACE --timeout=60s
echo "Helper pod ready."

# ── Utility functions ──────────────────────────────────────────────────
now_ns() { date +%s%N; }

# Run curl from inside the cluster (no network overhead for pod IPs)
ccurl() {
    kubectl exec $HELPER_POD -n $NAMESPACE -- curl "$@" 2>/dev/null
}

# Find the retransmitter pod (any status except Terminating)
find_retransmitter() {
    kubectl get pods -n $NAMESPACE -l "$SERVICE_LABEL" \
        -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.phase}{"\n"}{end}' 2>/dev/null | \
        grep -v Terminating | head -1 | awk '{print $1}' || true
}

# Get pod field
pod_field() {
    kubectl get pod "$1" -n $NAMESPACE -o jsonpath="$2" 2>/dev/null
}

# Find freeze daemon IP on a given node
freeze_ip_on() {
    local node=$1
    kubectl get pods -n $KN_NS -o wide 2>/dev/null | \
        grep freeze | grep "$node" | grep Running | awk '{print $6}'
}

# ── Main benchmark loop ───────────────────────────────────────────────
cold_starts=()
freeze_times=()
restore_times=()

for i in $(seq 1 "$ITERATIONS"); do
    echo ""
    echo "── Iteration $i / $ITERATIONS ──────────────────────────"

    # ── 1. COLD START ──────────────────────────────────────────
    # Delete ALL retransmitter pods by label to get a clean cold start.
    echo "  Deleting all retransmitter pods..."

    # Snapshot current pod names before deletion
    EXISTING=$(kubectl get pods -n $NAMESPACE -l "$SERVICE_LABEL" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)

    T0=$(now_ns)
    kubectl delete pods -n $NAMESPACE -l "$SERVICE_LABEL" --wait=false 2>/dev/null || true

    # Wait for a completely new pod to be Running and serving HTTP
    echo -n "  Waiting for new pod..."
    while true; do
        NEW_POD=""
        for candidate in $(kubectl get pods -n $NAMESPACE -l "$SERVICE_LABEL" \
            --field-selector=status.phase=Running \
            -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null); do
            if ! echo "$EXISTING" | grep -qxF "$candidate"; then
                NEW_POD="$candidate"
                break
            fi
        done

        if [ -n "$NEW_POD" ]; then
            NEW_IP=$(pod_field "$NEW_POD" '{.status.podIP}')
            if [ -n "$NEW_IP" ]; then
                CODE=$(ccurl -s -o /dev/null -w "%{http_code}" --max-time 2 "http://${NEW_IP}:8080/" || true)
                if [[ "$CODE" == "405" || "$CODE" == "200" ]]; then
                    T1=$(now_ns)
                    COLD_MS=$(( (T1 - T0) / 1000000 ))
                    echo " ${COLD_MS}ms"
                    break
                fi
            fi
        fi
        echo -n "."
        sleep 0.5
    done

    # Let the pod settle and start receiving events
    echo "  Settling for ${SETTLE_TIME}s..."
    sleep "$SETTLE_TIME"

    # ── 2. FREEZE (CRIU Checkpoint) ────────────────────────────
    POD_NAME="$NEW_POD"
    POD_IP="$NEW_IP"
    NODE=$(pod_field "$POD_NAME" '{.spec.nodeName}')
    FRZIP=$(freeze_ip_on "$NODE")
    echo "  Pod: $POD_NAME on $NODE (freeze daemon: $FRZIP)"

    echo -n "  Freezing..."
    FREEZE_SEC=$(ccurl -s -o /dev/null \
        -w "%{time_total}" \
        -X POST "http://${FRZIP}:8080/" \
        -H 'Content-Type: application/json' \
        -d "{\"action\":\"pause\",\"podName\":\"${POD_NAME}\",\"namespace\":\"${NAMESPACE}\"}" \
        --max-time 120)
    FREEZE_MS=$(python3 -c "print(int(float('${FREEZE_SEC}') * 1000))")
    echo " ${FREEZE_MS}ms"

    # ── 3. RESTORE (CRIU Restore) ──────────────────────────────
    # The thaw request is synchronous: it returns after CRIU restore
    # completes and the process is running again. We use curl's
    # time_total as the restore time, same as for freeze.
    echo -n "  Restoring..."
    RESTORE_SEC=$(ccurl -s -o /dev/null \
        -w "%{time_total}" \
        -X POST "http://${FRZIP}:8080/" \
        -H 'Content-Type: application/json' \
        -d "{\"action\":\"resume\",\"podName\":\"${POD_NAME}\",\"namespace\":\"${NAMESPACE}\"}" \
        --max-time 120)
    RESTORE_MS=$(python3 -c "print(int(float('${RESTORE_SEC}') * 1000))")
    echo " ${RESTORE_MS}ms"

    # Record
    echo "$i,$COLD_MS,$FREEZE_MS,$RESTORE_MS" >> "$OUTPUT"
    cold_starts+=("$COLD_MS")
    freeze_times+=("$FREEZE_MS")
    restore_times+=("$RESTORE_MS")

    echo "  ✓ Cold: ${COLD_MS}ms | Freeze: ${FREEZE_MS}ms | Restore: ${RESTORE_MS}ms"
done

echo ""
echo "=== Results saved to $OUTPUT ==="
echo ""

# ── Statistics ─────────────────────────────────────────────────────────
python3 - "$OUTPUT" << 'PYEOF'
import csv, sys, statistics

with open(sys.argv[1]) as f:
    reader = csv.DictReader(f)
    rows = list(reader)

cold = [int(r["cold_start_ms"]) for r in rows]
freeze = [int(r["freeze_ms"]) for r in rows]
restore = [int(r["restore_ms"]) for r in rows]

def stats(name, data):
    print(f"\n{name} ({len(data)} samples):")
    print(f"  Mean:   {statistics.mean(data):.1f} ms")
    print(f"  Median: {statistics.median(data):.1f} ms")
    print(f"  Stdev:  {statistics.stdev(data):.1f} ms")
    print(f"  Min:    {min(data)} ms")
    print(f"  Max:    {max(data)} ms")
    p95 = sorted(data)[int(len(data)*0.95)]
    print(f"  P95:    {p95} ms")

stats("Cold Start", cold)
stats("CRIU Freeze (Checkpoint)", freeze)
stats("CRIU Restore", restore)

speedup = statistics.mean(cold) / statistics.mean(restore)
print(f"\n{'='*50}")
print(f"  Speedup (cold start / restore): {speedup:.2f}x")
print(f"{'='*50}")
PYEOF
