package phpfpm

import (
	"fmt"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// k8sGetClient returns a Kubernetes clientset to interact with the cluster.
// This is intended to be used when the application is running inside a Kubernetes pod.
func k8sGetClient() (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to create in-cluster config: %v", err)
	}

	// Create a Kubernetes clientset using the in-cluster config
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes clientset: %v", err)
	}

	return clientset, nil
}
