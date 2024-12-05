package phpfpm

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	labelKey = "php-fpm-exporter/collect"
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

// DiscoverPods finds pods with the specified annotation in the given namespace.
func DiscoverPods(namespace string, pm *PoolManager, port string) error {
	// Get the Kubernetes client
	clientset, err := k8sGetClient()
	if err != nil {
		return err
	}

	// Watch for changes in the pods
	podWatch, err := clientset.CoreV1().Pods(namespace).Watch(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=true", labelKey),
	})
	if err != nil {
		return fmt.Errorf("failed to initialize watch session: %s", err)
	}

	var (
		podPhases   = make(map[string]v1.PodPhase)
		uriTemplate = fmt.Sprintf("tcp://%%s:%s/status", port)
	)
	// Watch for pod events
	go func() {
		for event := range podWatch.ResultChan() {
			pod, ok := event.Object.(*v1.Pod)
			if !ok {
				log.Errorf("Unexpected type in podWatch: %v", event.Object)
				continue
			}

			podName := pod.Name
			currentPhase := pod.Status.Phase

			log.Debugf("Received event for pod: %s, type: %s, current phase: %s", podName, event.Type, currentPhase)

			switch event.Type {
			case "ADDED":
				// Initialize the pod's phase in the map
				podPhases[podName] = currentPhase

				if currentPhase == v1.PodRunning {
					ip := pod.Status.PodIP
					if ip != "" {
						uri := fmt.Sprintf(uriTemplate, ip)
						log.Infof("New pod %s added and already Running with IP %s", podName, ip)
						pm.Add(uri)
					} else {
						log.Debugf("Pod %s added but has no IP yet", podName)
					}
				}

			case "MODIFIED":
				// Check for the Pending → Running transition
				lastPhase, exists := podPhases[podName]
				if exists && lastPhase == v1.PodPending && currentPhase == v1.PodRunning {
					log.Infof("Pod %s transitioned from Pending to Running", podName)

					ip := pod.Status.PodIP
					if ip != "" {
						uri := fmt.Sprintf(uriTemplate, ip)
						log.Infof("Adding Running pod %s with IP %s", podName, ip)
						pm.Add(uri)
					} else {
						log.Debugf("Pod %s is Running but has no IP assigned", podName)
					}
				}
				podPhases[podName] = currentPhase

			case "DELETED":
				delete(podPhases, podName)

				ip := pod.Status.PodIP
				if ip != "" {
					uri := fmt.Sprintf(uriTemplate, ip)
					log.Infof("Removing pod %s with IP %s from PoolManager", podName, ip)
					pm.Remove(uri)
				}
			}
		}
	}()
	return nil
}
