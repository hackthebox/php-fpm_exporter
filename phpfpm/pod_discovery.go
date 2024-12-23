package phpfpm

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apiWatch "k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/watch"
)

const uriTemplate string = "tcp://%s:%s/status"

type customWatcher struct {
	clientset     *kubernetes.Clientset
	labelSelector string
	namespace     string
}

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

// Watch starts a new watch session for Pods
func (c *customWatcher) Watch(options metav1.ListOptions) (apiWatch.Interface, error) {
	if c.namespace == "" {
		c.namespace = metav1.NamespaceAll
	}
	options.LabelSelector = c.labelSelector
	return c.clientset.CoreV1().Pods(c.namespace).Watch(context.TODO(), options)
}

// DiscoverPods finds pods with the specified annotation in the given namespace.
func (pm *PoolManager) DiscoverPods(namespace string, podLabels string, port string, exporter *Exporter) error {
	// Get the Kubernetes client
	clientset, err := k8sGetClient()
	if err != nil {
		return err
	}

	log.Info("Test 1.9.14")

	var podPhases = make(map[string]v1.PodPhase)

	podList, _ := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: podLabels})
	// Watch for pod events
	go func() {
		retryWatcher, err := watch.NewRetryWatcher(podList.ResourceVersion, &customWatcher{clientset: clientset, namespace: namespace, labelSelector: podLabels})
		if err != nil {
			log.Errorf("Failed to create RetryWatcher: %v", err)
		}
		defer retryWatcher.Stop()
		log.Info("RetryWatcher successfully initialized")

		for event := range retryWatcher.ResultChan() {
			pod, ok := event.Object.(*v1.Pod)
			if !ok {
				log.Errorf("Unexpected type in podWatch: %v", event.Object)
				continue
			}
			log.Debug("I am inside the go routine")
			log.Debugf("Pod labels: %s\nPort: %s", podLabels, port)

			podName := pod.Name
			currentPhase := pod.Status.Phase

			log.Debugf("Received event for pod: %s, type: %s, current phase: %s", podName, event.Type, currentPhase)

			switch event.Type {
			case apiWatch.Added:
				// Initialize the pod's phase in the map
				podPhases[podName] = currentPhase

				if currentPhase == v1.PodRunning {
					ip := pod.Status.PodIP
					if ip != "" {
						uri := fmt.Sprintf(uriTemplate, ip, port)
						log.Infof("New pod %s added and already Running with IP %s", podName, ip)
						pm.Add(uri)
						exporter.UpdatePoolManager(*pm)
					} else {
						log.Debugf("Pod %s added but has no IP yet", podName)
					}
				}

			case apiWatch.Modified:
				// Check for the Pending → Running transition
				lastPhase, exists := podPhases[podName]
				if exists && lastPhase == v1.PodPending && currentPhase == v1.PodRunning {
					log.Infof("Pod %s transitioned from Pending to Running", podName)

					ip := pod.Status.PodIP
					if ip != "" {
						uri := fmt.Sprintf(uriTemplate, ip, port)
						log.Infof("Adding Running pod %s with IP %s", podName, ip)
						pm.Add(uri)
						exporter.UpdatePoolManager(*pm)
					} else {
						log.Debugf("Pod %s is Running but has no IP assigned", podName)
					}
				}
				podPhases[podName] = currentPhase

			case apiWatch.Deleted:
				delete(podPhases, podName)

				ip := pod.Status.PodIP
				if ip != "" {
					uri := fmt.Sprintf(uriTemplate, ip, port)
					log.Infof("Removing pod %s with IP %s from PoolManager", podName, ip)
					pm.Remove(uri, exporter)
				}
			}
		}
	}()
	return nil
}
