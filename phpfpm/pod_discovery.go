package phpfpm

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiWatch "k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/watch"
)

const uriTemplate string = "tcp://%s:%s/status"

// Delay before relisting after the watch has ended. It doubles on every
// consecutive relist up to the maximum, and resets once a watch has outlived
// the maximum. Variables rather than constants so tests need not wait.
var (
	relistInitialDelay = time.Second
	relistMaxDelay     = 30 * time.Second
)

// customWatcher is a custom implementation of the cache.Watcher interface,
// designed to watch Kubernetes pods based on specific label selectors and namespace.
type customWatcher struct {
	clientset     *kubernetes.Clientset
	labelSelector string
	namespace     string
}

// newWatcher creates and returns a new instance of customWatcher.
// It is used to initialize the watcher with a Kubernetes clientset, a namespace, and a label selector for filtering the pods to be monitored.
func newWatcher(clientset *kubernetes.Clientset, namespace string, podLabels string) cache.WatcherWithContext {
	return &customWatcher{
		clientset:     clientset,
		namespace:     namespace,
		labelSelector: podLabels,
	}
}

// WatchWithContext initiates a new watch session for Pods by establishing a connection to the Kubernetes API.
// This function is used as part of the NewRetryWatcherWithContext setup, which ensures a resilient connection.
// If the connection to the API is interrupted, the NewRetryWatcherWithContext will automatically attempt to re-establish it,
// providing continuous monitoring of pod events. This approach is ideal for maintaining reliable event streaming,
// especially in cases of network instability or API server disruptions.
// Cancelling ctx ends the watch, which is how server shutdown stops pod discovery.
func (c *customWatcher) WatchWithContext(ctx context.Context, options metav1.ListOptions) (apiWatch.Interface, error) {
	options.LabelSelector = c.labelSelector
	result, err := c.clientset.CoreV1().Pods(c.namespace).Watch(ctx, options)
	if apierrors.IsGone(err) || apierrors.IsResourceExpired(err) {
		// The RetryWatcher stops on a 410 that arrives as a watch event, but
		// retries forever, with the same expired resource version, on a 410
		// that is the HTTP response to opening the watch. Hand it the event
		// form, so that both end the watch and runDiscovery relists.
		expired := apierrors.NewResourceExpired(err.Error()).ErrStatus
		events := make(chan apiWatch.Event, 1)
		events <- apiWatch.Event{Type: apiWatch.Error, Object: &expired}
		return apiWatch.NewProxyWatcher(events), nil
	}
	return result, err
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
func listPods(ctx context.Context, clientset *kubernetes.Clientset, namespace string, podLabels string) (*v1.PodList, error) {
	if namespace == "" {
		namespace = metav1.NamespaceAll
	}
	podList, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: podLabels})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}
	return podList, nil
}

// initializePodEnlisting retrieves all pods matching the specified criteria and appends their URIs to the PoolManager's PodPhases.
// This function is invoked prior to starting the NewRetryWatcher to capture the initial state of existing pods
// and to obtain the ResourceVersion required for initializing the NewRetryWatcher.
func (pm *PoolManager) initialPodEnlisting(exporter *Exporter, podList *v1.PodList, port string) string {

	log.Infof("Found %d pod(s) during initial list", len(podList.Items))
	for _, pod := range podList.Items {
		podName := pod.Name
		currentPhase := pod.Status.Phase
		log.Debugf("Processing pod from initial list: %s, phase: %s", podName, currentPhase)

		uri := fmt.Sprintf(uriTemplate, pod.Status.PodIP, port)
		pm.processPodAdded(exporter, &pod, uri)
	}
	return podList.ResourceVersion
}

// handlePodRunning is used when a pod is in the Running phase and needs to be appended into the pool manager's PodPhases.
func (pm *PoolManager) handlePodRunning(exporter *Exporter, pod *v1.Pod, uri string) {
	ip := pod.Status.PodIP
	podName := pod.Name
	if ip != "" {
		log.Infof("Pod in Running state detected %s with IP %s. Adding in the Pool Manager..", podName, ip)
		pm.Add(uri, podName)
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
// It then starts a watch session in a separate goroutine, which relists and resumes whenever the watch ends (see runDiscovery).
// The list operation is performed first to retrieve the initial ResourceVersion, which is required to initialize a NewRetryWatcherWithContext.
func (pm *PoolManager) DiscoverPods(ctx context.Context, exporter *Exporter, namespace string, podLabels string, port string) error {
	// Get the Kubernetes client
	clientset, err := k8sGetClient()
	if err != nil {
		return err
	}

	return pm.discoverPods(ctx, exporter, clientset, namespace, podLabels, port)
}

// discoverPods is DiscoverPods with the client supplied, so the discovery flow
// can be exercised against a test API server. k8sGetClient needs in-cluster
// config and so cannot run outside a pod.
func (pm *PoolManager) discoverPods(ctx context.Context, exporter *Exporter, clientset *kubernetes.Clientset, namespace string, podLabels string, port string) error {
	watcher := newWatcher(clientset, namespace, podLabels)

	podList, err := listPods(ctx, clientset, namespace, podLabels)
	if err != nil {
		return err
	}

	initialResourceVersion := pm.initialPodEnlisting(exporter, podList, port)
	exporter.discoveryUp.Store(true)

	go pm.runDiscovery(ctx, exporter, clientset, watcher, namespace, podLabels, initialResourceVersion, port)
	return nil
}

// runDiscovery keeps pod discovery alive until ctx is cancelled. The
// RetryWatcher reconnects on its own after most failures, but it gives up for
// good on a 410 Gone (resource version too old): it forwards the error and
// closes its channel. Whenever the watch ends, this relists, reconciles the
// pools against the pods that exist now, and watches again from the new
// resource version. While the watch is down, phpfpm_k8s_discovery_up reports 0.
func (pm *PoolManager) runDiscovery(ctx context.Context, exporter *Exporter, clientset *kubernetes.Clientset, watcher cache.WatcherWithContext, namespace string, podLabels string, resourceVersion string, port string) {
	delay := relistInitialDelay

	for {
		started := time.Now()
		pm.watchPodEvents(ctx, exporter, watcher, resourceVersion, port)
		if ctx.Err() != nil {
			return
		}
		exporter.discoveryUp.Store(false)

		if time.Since(started) > relistMaxDelay {
			delay = relistInitialDelay
		}

		for {
			log.Errorf("Pod watch ended, relisting pods in %v", delay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			delay = min(delay*2, relistMaxDelay)

			podList, err := listPods(ctx, clientset, namespace, podLabels)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Errorf("Pod discovery is stalled until a relist succeeds: %v", err)
				continue
			}

			resourceVersion = pm.reconcilePods(exporter, podList, port)
			break
		}

		exporter.discoveryRelists.Add(1)
		exporter.discoveryUp.Store(true)
	}
}

// reconcilePods makes the pools match a fresh pod list. Events that happened
// while the watch was down are lost, so the list is the only source of truth:
// pools whose pod is gone are removed, and running pods without a pool are
// added. Pools that are still valid are left alone, which keeps their scrape
// failure counters. It returns the list's ResourceVersion for the next watch.
func (pm *PoolManager) reconcilePods(exporter *Exporter, podList *v1.PodList, port string) string {
	phases := make(map[string]v1.PodPhase, len(podList.Items))
	wanted := make(map[string]string, len(podList.Items)) // scrape URI -> pod name

	for i := range podList.Items {
		pod := &podList.Items[i]
		phases[pod.Name] = pod.Status.Phase
		if pod.Status.Phase == v1.PodRunning && pod.Status.PodIP != "" {
			wanted[fmt.Sprintf(uriTemplate, pod.Status.PodIP, port)] = pod.Name
		}
	}

	var stale []string
	kept := make(map[string]bool, len(pm.Pools))
	for i := range pm.Pools {
		uri, podName := pm.Pools[i].Address, pm.Pools[i].Pod
		if name, ok := wanted[uri]; ok && name == podName {
			kept[uri] = true
			continue
		}
		stale = append(stale, uri)
	}

	for _, uri := range stale {
		log.Infof("Removing pool %s: its pod is gone after relist", uri)
		pm.Remove(exporter, uri)
		delete(kept, uri)
	}

	added := 0
	for i := range podList.Items {
		pod := &podList.Items[i]
		uri := fmt.Sprintf(uriTemplate, pod.Status.PodIP, port)
		if _, ok := wanted[uri]; !ok || kept[uri] {
			continue
		}
		log.Infof("Adding pod %s with IP %s found by relist", pod.Name, pod.Status.PodIP)
		pm.Add(uri, pod.Name)
		kept[uri] = true
		added++
	}

	pm.PodPhases = phases
	exporter.UpdatePoolManager(*pm)

	log.Infof("Relisted %d pod(s): removed %d stale pool(s), added %d", len(podList.Items), len(stale), added)
	return podList.ResourceVersion
}

// watchPodEvents monitors pod events and processes them accordingly:
// - For "added" events, the new pod's URI is appended to the pool manager.
// - For "modified" events, it verifies if the pod is in the running state before appending its URI to the pool manager.
// - For "deleted" events, the pod's URI is removed from the pool manager's PodPhases.
// It returns when the watch ends, either because ctx was cancelled or because the RetryWatcher gave up; runDiscovery decides what happens next.
func (pm *PoolManager) watchPodEvents(ctx context.Context, exporter *Exporter, watcher cache.WatcherWithContext, resourceVersion string, port string) {
	retryWatcher, err := watch.NewRetryWatcherWithContext(ctx, resourceVersion, watcher)
	if err != nil {
		log.Errorf("Failed to create Retry Watcher: %v", err)
		return
	}
	defer retryWatcher.Stop()
	log.Info("Retry Watcher initialized successfully")

	for event := range retryWatcher.ResultChan() {
		if event.Type == apiWatch.Error {
			// The RetryWatcher only forwards errors it will not retry, such as
			// a 410 Gone, and closes the channel next. runDiscovery relists.
			log.Errorf("Pod watch failed: %v", apierrors.FromObject(event.Object))
			continue
		}

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
