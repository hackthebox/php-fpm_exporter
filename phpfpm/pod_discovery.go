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

type customWatcher struct {
	clientset     *kubernetes.Clientset
	labelSelector string
	namespace     string
}

// newWatcher creates a new instance of customWatcher.
func newWatcher(clientset *kubernetes.Clientset, namespace string, podLabels string) cache.Watcher {
	return &customWatcher{
		clientset:     clientset,
		namespace:     namespace,
		labelSelector: podLabels,
	}
}

// Watch starts a new watch session for Pods
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

// listPods returns the initial list of pods
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

// initializePodEnlisting lists all Pods that match the criteria and initializes their state in PoolManager.
func (pm *PoolManager) initialPodEnlisting(exporter *Exporter, podList *v1.PodList, port string) (string, error) {

	log.Infof("Found %d pods during initial list", len(podList.Items))
	for _, pod := range podList.Items {
		podName := pod.Name
		currentPhase := pod.Status.Phase
		log.Debugf("Processing pod from initial list: %s, phase: %s", podName, currentPhase)

		uri := fmt.Sprintf(uriTemplate, pod.Status.PodIP, port)
		pm.processPodAdded(exporter, &pod, uri)
	}
	return podList.ResourceVersion, nil
}

// handlePodRunning handles actions for a pod that is in the Running phase.
func (pm *PoolManager) handlePodRunning(exporter *Exporter, pod *v1.Pod, uri string) {
	ip := pod.Status.PodIP
	podName := pod.Name
	if ip != "" {
		log.Infof("Handling Running pod %s with IP %s", podName, ip)
		pm.Add(uri)
		exporter.UpdatePoolManager(*pm)
	} else {
		log.Debugf("Pod %s is Running but has no IP assigned", podName)
	}
}

// podAdded processes a newly added pod.
func (pm *PoolManager) processPodAdded(exporter *Exporter, pod *v1.Pod, uri string) {
	pm.PodPhases[pod.Name] = pod.Status.Phase

	if pod.Status.Phase == v1.PodRunning {
		pm.handlePodRunning(exporter, pod, uri)
	}
}

// podModified processes modifications to an existing pod.
func (pm *PoolManager) processPodModified(exporter *Exporter, pod *v1.Pod, uri string) {
	lastPhase, exists := pm.PodPhases[pod.Name]

	if exists && lastPhase == v1.PodPending && pod.Status.Phase == v1.PodRunning {
		log.Infof("Pod %s transitioned from Pending to Running", pod.Name)
		pm.handlePodRunning(exporter, pod, uri)
	}
	pm.PodPhases[pod.Name] = pod.Status.Phase
}

// podDeleted processes the deletion of a pod.
func (pm *PoolManager) processPodDeleted(exporter *Exporter, pod *v1.Pod, uri string) {
	ip := pod.Status.PodIP

	log.Infof("Removing pod %s with IP %s from PoolManager", pod.Name, ip)
	pm.Remove(exporter, uri)

	delete(pm.PodPhases, pod.Name)
}

// DiscoverPods finds pods with the specified annotation in the given namespace.
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

// watchPodEvents watches for pod events and processes them.
func (pm *PoolManager) watchPodEvents(exporter *Exporter, watcher cache.Watcher, resourceVersion string, port string) {
	retryWatcher, err := watch.NewRetryWatcher(resourceVersion, watcher)
	if err != nil {
		log.Errorf("Failed to create RetryWatcher: %v", err)
		return
	}
	defer retryWatcher.Stop()
	log.Info("RetryWatcher initialized successfully")

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
