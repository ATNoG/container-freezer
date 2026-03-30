package common

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	cri "k8s.io/cri-api/pkg/apis/runtime/v1"
)

var ErrNoNonQueueProxyPods = errors.New("no non queue-proxy containers found in pod")

// List returns container IDs for a pod identified as "namespace/podName".
func List(ctx context.Context, conn *grpc.ClientConn, podKey string) ([]string, error) {
	parts := strings.SplitN(podKey, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("podKey must be namespace/podName, got: %s", podKey)
	}
	namespace, podName := parts[0], parts[1]

	client := cri.NewRuntimeServiceClient(conn)
	pods, err := client.ListPodSandbox(context.Background(), &cri.ListPodSandboxRequest{
		Filter: &cri.PodSandboxFilter{
			LabelSelector: map[string]string{
				"io.kubernetes.pod.name":      podName,
				"io.kubernetes.pod.namespace": namespace,
			},
		},
	})
	if err != nil {
		return nil, err
	}

	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("pod %s not found", podKey)
	}
	pod := pods.Items[0]

	ctrs, err := client.ListContainers(ctx, &cri.ListContainersRequest{Filter: &cri.ContainerFilter{
		PodSandboxId: pod.Id,
	}})
	if err != nil {
		return nil, err
	}

	return lookupContainerIDs(ctrs)
}

func lookupContainerIDs(ctrs *cri.ListContainersResponse) ([]string, error) {
	ids := make([]string, 0, len(ctrs.Containers)-1)
	for _, c := range ctrs.Containers {
		if c.GetMetadata().GetName() != "queue-proxy" {
			ids = append(ids, c.Id)
		}
	}
	if len(ids) == 0 {
		return nil, ErrNoNonQueueProxyPods
	}
	return ids, nil
}
