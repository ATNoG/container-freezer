# container-freezer-criu

Fork of [knative-sandbox/container-freezer](https://github.com/knative-sandbox/container-freezer) (archived April 2023) that replaces the **cgroup freezer** with **CRIU checkpoint/restore** for full RAM reclamation on Multi-access Edge Computing (MEC) nodes.

**Companion component:** [knative-freezer-plugin](https://github.com/ATNoG/knative-freezer-plugin) - the custom queue-proxy that detects idle containers and triggers freeze/thaw automatically.

## What Changed from the Original

The original container-freezer uses the Linux cgroup freezer to pause container processes in place — they stop executing but remain in memory. This fork replaces that mechanism with [CRIU](https://criu.org/) (Checkpoint/Restore In Userspace), which:

1. **Dumps full process state** (memory pages, file descriptors, TCP connections, CPU registers) to disk
2. **Kills the process** to free all RAM
3. **Restores from disk** when the function is needed again, resuming execution from the exact instruction where it was checkpointed

This is particularly useful for edge nodes with limited resources, where idle serverless functions should release memory for other workloads.

### Key Differences

| Aspect | Original (cgroup freeze) | This Fork (CRIU) |
|--------|--------------------------|-------------------|
| Mechanism | Write to cgroup `freezer.state` | `criu dump` / `criu restore` via containerd |
| RAM during idle | Fully consumed | Freed |
| TCP connections | Remain open (frozen) | Dumped and restored (`--tcp-established`) |
| Process lifecycle | Stays alive (suspended) | Killed → restored from checkpoint |
| Post-restore logging | N/A | CRI-formatted log writer for `kubectl logs` |
| Restart prevention | Not needed | Kyverno policy injects `restartPolicy: Never` |

### Performance (retransmitter service, 46 iterations)

| | Mean | Median | Stdev | Min | Max | P95 |
|---|---|---|---|---|---|---|
| **Cold Start** | 5258 ms | 5043 ms | 25425 ms* | 3827 ms | 177697 ms* | 8559 ms |
| **CRIU Freeze** | 4389 ms | 4269 ms | 575 ms | 3267 ms | 5832 ms | 5373 ms |
| **CRIU Restore** | 641 ms | 637 ms | 30 ms | 547 ms | 705 ms | 697 ms |
| **Speedup** | **8.2x** | | | | | |

\*Cold start outlier at iteration 12 (177s, likely image pull). Restore is very consistent (~641ms ± 30ms).

## Architecture

```
┌─────────────────────────────────────────────────┐
│  Knative Queue-Proxy (freezer plugin)           │
│  Detects idle → calls freeze daemon             │
│  Detects request → calls thaw before forwarding │
└──────────────────────┬──────────────────────────┘
                       │ HTTP POST :8080
                       ▼
┌─────────────────────────────────────────────────┐
│  Freeze Daemon (DaemonSet, one per node)        │
│  pause  → containerd → runc → criu dump         │
│  resume → containerd → runc → criu restore      │
│                                                  │
│  Post-restore IO: CRI log writer + shared FIFOs │
└──────────────────────┬──────────────────────────┘
                       │
                       ▼
┌─────────────────────────────────────────────────┐
│  containerd + CRIU (installed on worker node)   │
│  Checkpoint stored in containerd content store  │
└─────────────────────────────────────────────────┘
```

## Prerequisites

- Kubernetes cluster with **containerd** runtime
- **CRIU** installed on all worker nodes where checkpoint/restore will run (amd64 only)
- **Knative Serving** installed
- **Kyverno** installed (for `restartPolicy: Never` injection)
- Nodes labelled: `kubectl label node <node> knative.dev/container-runtime=containerd`

## Installation

### 1. Install CRIU on worker nodes

```bash
sudo apt update && sudo apt install -y \
  build-essential pkg-config libnet1-dev libprotobuf-dev \
  libprotobuf-c-dev protobuf-c-compiler protobuf-compiler \
  python3-protobuf libnl-3-dev libcap-dev uuid-dev libnftables-dev

git clone https://github.com/checkpoint-restore/criu.git
cd criu && make -j$(nproc) && sudo make install-criu
sudo criu check --all
```

> CRIU only works on amd64. ARM64 requires kernel recompilation.

### 2. Deploy the freeze daemon

```bash
# RBAC and ConfigMap
kubectl apply -f config/common/

# DaemonSet (includes volume mounts for CRIU logging and FIFOs)
kubectl apply -f config/containerd/300-daemon-containerd.yaml
```

### 3. Deploy the Kyverno restart policy

After CRIU checkpoint kills the container, `restartPolicy: Never` prevents kubelet from restarting it. Knative's webhook strips this field, so Kyverno re-injects it:

```bash
kubectl apply -f kyverno/inject-restart-policy-never.yaml
```

### 4. Build and push the daemon image

```bash
./build.sh          # pushes ghcr.io/pmacoutinho/freezer-daemon:latest
./build.sh v1.0.0   # or with a specific tag
```

Requires a Docker buildx builder named `mec-builder`:
```bash
docker buildx create --name mec-builder --driver docker-container --use
```

### 5. Deploy the queue-proxy plugin

The [knative-freezer-plugin](https://github.com/ATNoG/knative-freezer-plugin) replaces Knative's stock queue-proxy with one that automatically freezes idle containers and thaws them on incoming requests. See its README for build and deployment instructions.

```bash
# Quick start (from the knative-freezer-plugin repo):
./build.sh    # build and push the custom queue-proxy image
./patch.sh    # patch Knative to use it
```

### 6. Activate on a Knative Service

```bash
kubectl patch ksvc <service-name> --type merge -p \
  '{"spec":{"template":{"metadata":{"annotations":{"qpoption.knative.dev/freezer-activate":"enable"}}}}}'
```

## Manual Testing

Send freeze/thaw requests directly to the daemon for testing:

```bash
# Find the freeze daemon IP on the same node as your pod
FREEZE_IP=$(kubectl get pods -n knative-serving -l name=freeze-daemon-containerd -o wide | \
  grep <node-name> | awk '{print $6}')

# Freeze (CRIU checkpoint)
kubectl run curl-test --rm -i --restart=Never --image=curlimages/curl -- \
  curl -s -w "\nHTTP %{http_code}\n" -X POST "http://${FREEZE_IP}:8080/" \
  -H 'Content-Type: application/json' \
  -d '{"action":"pause","podName":"<pod-name>","namespace":"default"}' \
  --max-time 120

# Thaw (CRIU restore)
kubectl run curl-test --rm -i --restart=Never --image=curlimages/curl -- \
  curl -s -w "\nHTTP %{http_code}\n" -X POST "http://${FREEZE_IP}:8080/" \
  -H 'Content-Type: application/json' \
  -d '{"action":"resume","podName":"<pod-name>","namespace":"default"}' \
  --max-time 120
```

## Benchmarking

Run the integration benchmark (50 iterations by default):

```bash
./benchmark-integration.sh [iterations]
```

Outputs a CSV file with cold start, freeze, and restore times per iteration, plus summary statistics.

## Known Limitations

- **Pod shows Error after restore**: Kubelet doesn't know the container was restored (CRIU operates below kubelet). The container is fully functional — only the status display is wrong. Fixing this requires the Container Checkpoint/Restore API (KEP-2008), still in alpha.
- **`kubectl logs -f` doesn't follow after restore**: CRI considers the container terminated. Use `tail -f /var/log/pods/...` on the node for live log tracking.
- **Checkpoints stored in daemon memory**: If the daemon pod restarts between freeze and thaw, the checkpoint reference is lost. The pod will need a cold start.
- **amd64 only**: CRIU does not work on ARM64 without kernel recompilation.

## Project Structure

```
cmd/daemon/          # Daemon entry point
pkg/daemon/          # HTTP handler (pause/resume API)
pkg/freeze/          # CRI provider abstraction
pkg/freeze/containerd/  # CRIU checkpoint/restore implementation
pkg/freeze/crio/     # CRI-O implementation (unchanged from original)
pkg/freeze/common/   # Shared container listing logic
config/              # Kubernetes manifests (RBAC, DaemonSet, ConfigMap)
kyverno/             # Kyverno ClusterPolicy for restartPolicy injection
benchmark-integration.sh  # Cold start vs CRIU restore benchmark
```

## Related

- [knative-freezer-plugin](https://github.com/ATNoG/knative-freezer-plugin) — custom queue-proxy with automatic freeze/thaw (companion component)
- [knative-sandbox/container-freezer](https://github.com/knative-sandbox/container-freezer) — original upstream repo (archived)

## License

This project is licensed under the [GNU General Public License v3.0](LICENSE). Original upstream work by the Knative Authors under the Apache License 2.0.
