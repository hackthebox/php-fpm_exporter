package phpfpm

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiWatch "k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/watch"
)

const uriTemplate string = "tcp://%s:%s/status"

// customWatcher is a custom implementation of the cache.Watcher interface,
// designed to watch Kubernetes pods based on specific label selectors and namespace.
type customWatcher struct {
	clientset     *kubernetes.Clientset
	labelSelector string
	namespace     string
}

// newWatcher creates and returns a new instance of customWatcher.
// It is used to initialize the watcher with a Kubernetes clientset, a namespace, and a label selector for filtering the pods to be monitored.
func newWatcher(clientset *kubernetes.Clientset, namespace string, podLabels string) cache.Watcher {
	return &customWatcher{
		clientset:     clientset,
		namespace:     namespace,
		labelSelector: podLabels,
	}
}

// Watch initiates a new watch session for Pods by establishing a connection to the Kubernetes API.
// This function is used as part of the NewRetryWatcher setup, which ensures a resilient connection.
// If the connection to the API is interrupted, the NewRetryWatcher will automatically attempt to re-establish it,
// providing continuous monitoring of pod events. This approach is ideal for maintaining reliable event streaming,
// especially in cases of network instability or API server disruptions.
func (c *customWatcher) Watch(options metav1.ListOptions) (apiWatch.Interface, error) {
	options.LabelSelector = c.labelSelector
	ns := c.namespace
	if ns == "" {
		ns = metav1.NamespaceAll
	}
	return c.clientset.CoreV1().Pods(c.namespace).Watch(context.TODO(), options)
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

// listPods retrieves the initial list of pods that match the specified label criteria and namespace.
func listPods(clientset *kubernetes.Clientset, namespace string, podLabels string) (*v1.PodList, error) {
	if namespace == "" {
		namespace = metav1.NamespaceAll
	}
	podList, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: podLabels})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}
	return podList, nil
}

// initializePodEnlisting retrieves all pods matching the specified criteria and appends their URIs to the PoolManager's PodPhases.
// This function is invoked prior to starting the NewRetryWatcher to capture the initial state of existing pods
// and to obtain the ResourceVersion required for initializing the NewRetryWatcher.
func (pm *PoolManager) initialPodEnlisting(exporter *Exporter, podList *v1.PodList, port string) (string, error) {

	log.Infof("Found %d pod(s) during initial list", len(podList.Items))
	for _, pod := range podList.Items {
		podName := pod.Name
		currentPhase := pod.Status.Phase
		log.Debugf("Processing pod from initial list: %s, phase: %s", podName, currentPhase)

		uri := fmt.Sprintf(uriTemplate, pod.Status.PodIP, port)
		pm.processPodAdded(exporter, &pod, uri)
	}
	return podList.ResourceVersion, nil
}

// handlePodRunning is used when a pod is in the Running phase and needs to be appended into the pool manager's PodPhases.
func (pm *PoolManager) handlePodRunning(exporter *Exporter, pod *v1.Pod, uri string) {
	ip := pod.Status.PodIP
	podName := pod.Name
	if ip != "" {
		log.Infof("Pod in Running state detected %s with IP %s. Adding in the Pool Manager..", podName, ip)
		pm.Add(uri)
		exporter.UpdatePoolManager(*pm)
	} else {
		log.Debugf("Pod %s is in Running state but has no IP assigned", podName)
	}
}

// processPodAdded handles the addition of a newly created pod to the cluster by appending its URI to the pool manager.
func (pm *PoolManager) processPodAdded(exporter *Exporter, pod *v1.Pod, uri string) {
	pm.PodPhases[pod.Name] = pod.Status.Phase

	if pod.Status.Phase == v1.PodRunning {
		pm.handlePodRunning(exporter, pod, uri)
	}
}

// processPodModified handles events triggered by pod modifications, including when a new pod is added to the cluster.
// To be included in the pool manager, the pod must be in the "Running" phase. The function checks the pod's current phase
// and, if it is running, calls handlePodRunning to append the pod to the pool manager's PodPhases.
func (pm *PoolManager) processPodModified(exporter *Exporter, pod *v1.Pod, uri string) {
	podName := pod.Name
	currentPhase := pod.Status.Phase
	lastPhase, exists := pm.PodPhases[podName]

	if exists && lastPhase == v1.PodPending && currentPhase == v1.PodRunning {
		log.Infof("Pod %s transitioned from Pending to Running", podName)
		pm.handlePodRunning(exporter, pod, uri)
	}
	pm.PodPhases[podName] = currentPhase
}

// processPodDeleted handles the removal of a pods URI from the pool manager's PodPhases.
func (pm *PoolManager) processPodDeleted(exporter *Exporter, pod *v1.Pod, uri string) {
	ip := pod.Status.PodIP

	log.Infof("Removing pod %s with IP %s from PoolManager", pod.Name, ip)
	pm.Remove(exporter, uri)

	delete(pm.PodPhases, pod.Name)
}

// DiscoverPods begins by listing the pods that match the specified labels within the given namespace.
// It then starts a watch session in a separate goroutine.
// The list operation is performed first to retrieve the initial ResourceVersion, which is required to initialize a NewRetryWatcher.
func (pm *PoolManager) DiscoverPods(exporter *Exporter, namespace string, podLabels string, port string) error {
	// Get the Kubernetes client
	clientset, err := k8sGetClient()
	if err != nil {
		return err
	}

	watcher := newWatcher(clientset, namespace, podLabels)

	podList, err := listPods(clientset, namespace, podLabels)
	initialResourceVersion, err := pm.initialPodEnlisting(exporter, podList, port)
	if err != nil {
		return err
	}

	go pm.watchPodEvents(exporter, watcher, initialResourceVersion, port)
	return nil
}

// watchPodEvents monitors pod events and processes them accordingly:
// - For "added" events, the new pod's URI is appended to the pool manager.
// - For "modified" events, it verifies if the pod is in the running state before appending its URI to the pool manager.
// - For "deleted" events, the pod's URI is removed from the pool manager's PodPhases.
// Note: There is an unresolved issue with timeout errors when a pod is deleted, which requires further investigation and handling.
func (pm *PoolManager) watchPodEvents(exporter *Exporter, watcher cache.Watcher, resourceVersion string, port string) {
	retryWatcher, err := watch.NewRetryWatcher(resourceVersion, watcher)
	if err != nil {
		log.Errorf("Failed to create Retry Watcher: %v", err)
		return
	}
	defer retryWatcher.Stop()
	log.Info("Retry Watcher initialized successfully")

	for event := range retryWatcher.ResultChan() {
		pod, ok := event.Object.(*v1.Pod)
		if !ok {
			log.Errorf("Unexpected type in pod event: %v", event.Object)
			continue
		}

		uri := fmt.Sprintf(uriTemplate, pod.Status.PodIP, port)
		log.Debugf("Received event for pod %s: type=%s, phase=%s", pod.Name, event.Type, pod.Status.Phase)

		switch event.Type {
		case apiWatch.Added:
			pm.processPodAdded(exporter, pod, uri)
		case apiWatch.Modified:
			pm.processPodModified(exporter, pod, uri)
		case apiWatch.Deleted:
			pm.processPodDeleted(exporter, pod, uri)
		}
	}
}
