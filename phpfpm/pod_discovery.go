package phpfpm

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	annotationKey = "php-fpm-exporter/scrape"
)

// DiscoverPods finds pods with the specified annotation in the given namespace.
func DiscoverPods(namespace string, pm *PoolManager) error {
	// Get the Kubernetes client
	clientset, err := k8sGetClient()
	if err != nil {
		return err
	}
	err = getExistingPods(clientset, pm, namespace)
	if err != nil {
		return fmt.Errorf("failed to list existing pods: %v", err)
	}

	// Watch for changes in the pods
	podWatch, err := clientset.CoreV1().Pods(namespace).Watch(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=true", annotationKey),
	})
	if err != nil {
		return fmt.Errorf("failed to initialize watch session: %s", err)
	}

	// Watch for pod events
	go func() {
		for event := range podWatch.ResultChan() {
			pod, ok := event.Object.(*v1.Pod)
			if !ok {
				log.Errorf("Unexpected type in podWatch: %v", event.Object)
				continue
			}

			log.Debugf("Received event for pod: %s, type: %s", pod.Name, event.Type)
			log.Debugf("Namespace watching: %s", namespace)
			switch event.Type {
			case "ADDED":
				ip := pod.Status.PodIP
				uri := fmt.Sprintf("tcp://%s:8080/status", ip)
				log.Debugf("Pod added: %s with IP %s", pod.Name, ip)
				pm.Add(uri)

			case "DELETED":
				ip := pod.Status.PodIP
				uri := fmt.Sprintf("tcp://%s:8080/status", ip)
				log.Debugf("Pod deleted: %s with IP %s", pod.Name, ip)
				pm.Remove(uri)
			case "MODIFIED":
				ip := pod.Status.PodIP
				if ip != "" {
					uri := fmt.Sprintf("tcp://%s:8080/status", ip)
					log.Infof("Pod modified: %s with IP %s. Updating PoolManager.", pod.Name, ip)
					pm.Add(uri)
				} else {
					log.Debugf("Modified pod %s has no assigned IP, skipping...", pod.Name)
				}
			}
		}
	}()
	return nil
}

func getExistingPods(clientset *kubernetes.Clientset, pm *PoolManager, namespace string) error {
	pods, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=true", annotationKey),
	})
	if err != nil {
		return err
	}
	log.Infof("Retrieved pods list in namespace %s..", namespace)

	log.Infof("Pods list: %s", pods)

	for _, pod := range pods.Items {
		ip := pod.Status.PodIP
		if ip != "" {
			uri := fmt.Sprintf("tcp://%s:8080/status", ip)
			log.Infof("Adding existing pod: %s with IP %s", pod.Name, ip)
			pm.Add(uri)
		} else {
			log.Debugf("Pod %s has no assigned IP yet, skipping...", pod.Name)
		}
	}
	return nil
}
