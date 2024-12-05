package phpfpm

import (
	"context"
	"fmt"
	"strings"
	"time"

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
func DiscoverPods(namespace string, pm *PoolManager) error {
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

	// Watch for pod events
	go func() {
		for event := range podWatch.ResultChan() {
			pod, ok := event.Object.(*v1.Pod)
			if !ok {
				log.Errorf("Unexpected type in podWatch: %v", event.Object)
				continue
			}

			log.Debugf("Received event for pod: %s, type: %s", pod.Name, event.Type)
			switch event.Type {
			case "ADDED":
				go func(p *v1.Pod) {
					for {
						if p.Status.Phase == v1.PodRunning {
							ip := p.Status.PodIP
							if ip == "" {
								log.Debugf("Pod %s is running but has no IP assigned yet, retrying...", p.Name)
								time.Sleep(2 * time.Second)
								continue
							}
							uri := fmt.Sprintf("tcp://%s:8080/status", ip)
							log.Debugf("Pod added: %s with IP %s", p.Name, ip)
							pm.Add(uri)
							log.Debugf("Pools: %s", GetPoolAddresses(pm))
							return
						}
						log.Debugf("Pod %s is not in running state, current phase: %s. Retrying...", p.Name, p.Status.Phase)
						time.Sleep(10 * time.Second)
					}
				}(pod)

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

func GetPoolAddresses(pm *PoolManager) string {
	var addresses []string
	for _, pool := range pm.Pools {
		addresses = append(addresses, pool.Address)
	}
	return strings.Join(addresses, ", ")
}
