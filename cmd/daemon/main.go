package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/kelseyhightower/envconfig"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"knative.dev/container-freezer/pkg/daemon"
	"knative.dev/container-freezer/pkg/freeze"
	pkglogging "knative.dev/pkg/logging"
)

// checkpointSupportLabel is applied to this node when CRIU reports that the
// kernel supports process-level checkpoint/restore. The MEC operator reads it
// to decide whether to enable checkpoint-based idling (freeze) on the node.
const checkpointSupportLabel = "mec.atnog.org/checkpoint-support"

type config struct {
	RuntimeType string `split_words:"true" required:"true"`
	APIKey      string `split_words:"true"` // optional; if set, clients must send Authorization: Bearer <key>

	// Logging configuration
	FreezerLoggingConfig string `split_words:"true"`
	FreezerLoggingLevel  string `split_words:"true"`
}

// criuHostRoot is the mount prefix under which the host filesystem is exposed to
// this container, so the probe can look for the host's criu binary. The daemon
// manifest mounts host "/" read-only here. Overridable for testing.
var criuHostRoot = envOr("CRIU_HOST_ROOT", "/host")

// criuCandidatePaths are the standard host locations for the criu binary,
// resolved relative to criuHostRoot.
var criuCandidatePaths = []string{
	"/usr/sbin/criu",
	"/usr/local/sbin/criu",
	"/usr/bin/criu",
	"/usr/local/bin/criu",
	"/sbin/criu",
	"/bin/criu",
}

// hostHasCRIU reports whether the host has a criu binary in a standard location,
// as seen through the criuHostRoot mount. If the host root is not mounted (the
// prefix does not exist), it cannot prove absence, so it returns true and leaves
// the decision to the kernel check alone rather than failing every node.
func hostHasCRIU() bool {
	if _, err := os.Stat(criuHostRoot); err != nil {
		return true // host root not mounted; do not veto on an unmeasurable signal
	}
	for _, p := range criuCandidatePaths {
		if fi, err := os.Stat(filepath.Join(criuHostRoot, p)); err == nil && !fi.IsDir() {
			return true
		}
	}
	return false
}

// checkpointCapable reports whether this node can honor checkpoint/restore. It
// combines two dependency-free probes that work inside the distroless daemon
// image (which has no criu binary of its own):
//  1. KERNEL support: /proc/sys/kernel/ns_last_pid, which CRIU requires to
//     control the next PID on restore (gated by CONFIG_CHECKPOINT_RESTORE) and
//     which `criu check` itself tests.
//  2. USERSPACE presence: the host's criu binary, seen through the mounted host
//     root. The freeze path shells out through the container runtime to criu on
//     the host, so a node with kernel support but no criu binary would otherwise
//     be a false positive and get freeze enabled only to fail at runtime.
//
// It still does not verify the host's criu VERSION; see the design note.
func checkpointCapable() bool {
	if _, err := os.Stat("/proc/sys/kernel/ns_last_pid"); err != nil {
		return false // kernel lacks CONFIG_CHECKPOINT_RESTORE
	}
	return hostHasCRIU()
}

// envOr returns the value of environment variable key, or def when it is unset.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// labelNodeCheckpointSupport probes CRIU capability and patches this node with
// checkpointSupportLabel=<true|false>. It is best-effort: any failure is logged
// and ignored, and the operator treats a missing or "false" label as
// not-capable, so freeze is never enabled on a node that cannot honor it.
func labelNodeCheckpointSupport() {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		log.Printf("checkpoint-support labeling skipped: NODE_NAME not set (needs downward API spec.nodeName)")
		return
	}
	supported := checkpointCapable()

	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Printf("checkpoint-support labeling skipped: in-cluster config: %v", err)
		return
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Printf("checkpoint-support labeling skipped: clientset: %v", err)
		return
	}

	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, checkpointSupportLabel, strconv.FormatBool(supported))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := cs.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		log.Printf("checkpoint-support labeling: patch node %s failed: %v", nodeName, err)
		return
	}
	log.Printf("checkpoint-support: labeled node %s %s=%t", nodeName, checkpointSupportLabel, supported)
}

func main() {
	var env config
	if err := envconfig.Process("", &env); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	logger, _ := pkglogging.NewLogger(env.FreezerLoggingConfig, env.FreezerLoggingLevel)

	// Self-report checkpoint/restore capability as a node label so the MEC
	// operator only enables freeze on nodes that actually support it.
	labelNodeCheckpointSupport()

	freezeThaw, err := freeze.NewCRIProvider(env.RuntimeType)
	if err != nil {
		log.Fatal(err)
	}

	http.ListenAndServe(":8080", &daemon.Handler{
		Freezer: freezeThaw,
		Thawer:  freezeThaw,
		Logger:  logger,
		APIKey:  env.APIKey,
	})
}
