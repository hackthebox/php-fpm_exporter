// Copyright © 2018 Enrico Stahn <enrico.stahn@gmail.com>
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package phpfpm provides convenient access to PHP-FPM pool data
package phpfpm

import (
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	namespace = "phpfpm"
)

// Metric label names. Their spelling and order are a public contract.
const (
	labelPool      = "pool"
	labelPod       = "phpfpm_pod"
	labelChild     = "child"
	labelState     = "state"
	labelScrapeURI = "scrape_uri"
)

var (
	poolMetricLabelsWithPod         = []string{labelPool, labelPod, labelScrapeURI}
	processMetricLabelsWithPod      = []string{labelPool, labelPod, labelChild, labelScrapeURI}
	processStateMetricLabelsWithPod = []string{labelPool, labelPod, labelChild, labelState, labelScrapeURI}

	// Label sets for statically configured targets, which have no pod name.
	poolMetricLabelsNoPod         = []string{labelPool, labelScrapeURI}
	processMetricLabelsNoPod      = []string{labelPool, labelChild, labelScrapeURI}
	processStateMetricLabelsNoPod = []string{labelPool, labelChild, labelState, labelScrapeURI}
)

// Option configures an Exporter at construction. The metric label set is fixed
// once a descriptor exists, so options cannot be applied afterwards.
type Option func(*exporterOptions)

type exporterOptions struct {
	podLabel bool
}

// WithPodLabel adds the phpfpm_pod label to every metric. Only Kubernetes
// auto-tracking populates it; a static target has no pod name, and Prometheus
// treats an empty label value as equivalent to an absent one, so emitting
// phpfpm_pod="" would be noise. The choice is per exporter rather than per pool
// because client_golang rejects two descriptors that share a metric name but
// disagree on label names.
func WithPodLabel() Option {
	return func(o *exporterOptions) { o.podLabel = true }
}

func (e *Exporter) poolLabelValues(pool Pool) []string {
	if e.podLabel {
		return []string{pool.Name, pool.Pod, pool.Address}
	}
	return []string{pool.Name, pool.Address}
}

func (e *Exporter) processLabelValues(pool Pool, child string) []string {
	if e.podLabel {
		return []string{pool.Name, pool.Pod, child, pool.Address}
	}
	return []string{pool.Name, child, pool.Address}
}

func (e *Exporter) processStateLabelValues(pool Pool, child string, state string) []string {
	if e.podLabel {
		return []string{pool.Name, pool.Pod, child, state, pool.Address}
	}
	return []string{pool.Name, child, state, pool.Address}
}

// Exporter configures and exposes PHP-FPM metrics to Prometheus.
type Exporter struct {
	mutex       sync.Mutex
	PoolManager PoolManager

	CountProcessState bool

	podLabel bool

	up                       *prometheus.Desc
	scrapeFailues            *prometheus.Desc
	startSince               *prometheus.Desc
	acceptedConnections      *prometheus.Desc
	listenQueue              *prometheus.Desc
	maxListenQueue           *prometheus.Desc
	listenQueueLength        *prometheus.Desc
	idleProcesses            *prometheus.Desc
	activeProcesses          *prometheus.Desc
	totalProcesses           *prometheus.Desc
	maxActiveProcesses       *prometheus.Desc
	maxChildrenReached       *prometheus.Desc
	slowRequests             *prometheus.Desc
	processRequests          *prometheus.Desc
	processLastRequestMemory *prometheus.Desc
	processLastRequestCPU    *prometheus.Desc
	processRequestDuration   *prometheus.Desc
	processState             *prometheus.Desc
}

// NewExporter creates a new Exporter for a PoolManager and configures the necessary metrics.
// Pass WithPodLabel when the pools come from Kubernetes auto-tracking.
func NewExporter(pm PoolManager, opts ...Option) *Exporter {
	options := exporterOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	poolMetricLabels, processMetricLabels, processStateMetricLabels :=
		poolMetricLabelsNoPod, processMetricLabelsNoPod, processStateMetricLabelsNoPod
	if options.podLabel {
		poolMetricLabels, processMetricLabels, processStateMetricLabels =
			poolMetricLabelsWithPod, processMetricLabelsWithPod, processStateMetricLabelsWithPod
	}

	return &Exporter{
		PoolManager: pm,

		CountProcessState: false,

		podLabel: options.podLabel,

		up: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "up"),
			"Could PHP-FPM be reached?",
			poolMetricLabels,
			nil),

		scrapeFailues: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "scrape_failures"),
			"The number of failures scraping from PHP-FPM.",
			poolMetricLabels,
			nil),

		startSince: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "start_since"),
			"The number of seconds since FPM has started.",
			poolMetricLabels,
			nil),

		acceptedConnections: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "accepted_connections"),
			"The number of requests accepted by the pool.",
			poolMetricLabels,
			nil),

		listenQueue: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "listen_queue"),
			"The number of requests in the queue of pending connections.",
			poolMetricLabels,
			nil),

		maxListenQueue: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "max_listen_queue"),
			"The maximum number of requests in the queue of pending connections since FPM has started.",
			poolMetricLabels,
			nil),

		listenQueueLength: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "listen_queue_length"),
			"The size of the socket queue of pending connections.",
			poolMetricLabels,
			nil),

		idleProcesses: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "idle_processes"),
			"The number of idle processes.",
			poolMetricLabels,
			nil),

		activeProcesses: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "active_processes"),
			"The number of active processes.",
			poolMetricLabels,
			nil),

		totalProcesses: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "total_processes"),
			"The number of idle + active processes.",
			poolMetricLabels,
			nil),

		maxActiveProcesses: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "max_active_processes"),
			"The maximum number of active processes since FPM has started.",
			poolMetricLabels,
			nil),

		maxChildrenReached: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "max_children_reached"),
			"The number of times, the process limit has been reached, when pm tries to start more children (works only for pm 'dynamic' and 'ondemand').",
			poolMetricLabels,
			nil),

		slowRequests: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "slow_requests"),
			"The number of requests that exceeded your 'request_slowlog_timeout' value.",
			poolMetricLabels,
			nil),

		processRequests: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "process_requests"),
			"The number of requests the process has served.",
			processMetricLabels,
			nil),

		processLastRequestMemory: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "process_last_request_memory"),
			"The max amount of memory the last request consumed.",
			processMetricLabels,
			nil),

		processLastRequestCPU: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "process_last_request_cpu"),
			"The %cpu the last request consumed.",
			processMetricLabels,
			nil),

		processRequestDuration: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "process_request_duration"),
			"The duration in microseconds of the requests.",
			processMetricLabels,
			nil),

		processState: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "process_state"),
			"The state of the process (Idle, Running, ...).",
			processStateMetricLabels,
			nil),
	}
}

// Collect updates the Pools and sends the collected metrics to Prometheus
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	// Pool.error logs each failure with its address, and a failed scrape is
	// already visible as phpfpm_up 0 and phpfpm_scrape_failures, so logging the
	// joined error here would just say it twice.
	_ = e.PoolManager.Update()

	e.collectPools(ch, e.PoolManager.Pools)
}

func (e *Exporter) collectPools(ch chan<- prometheus.Metric, pools []Pool) {
	for _, pool := range pools {
		poolValues := e.poolLabelValues(pool)

		ch <- prometheus.MustNewConstMetric(e.scrapeFailues, prometheus.CounterValue, float64(pool.ScrapeFailures), poolValues...)

		if pool.ScrapeError != nil {
			ch <- prometheus.MustNewConstMetric(e.up, prometheus.GaugeValue, 0, poolValues...)
			log.Errorf("Error scraping PHP-FPM: %v", pool.ScrapeError)
			continue
		}

		active, idle, total := CountProcessState(pool.Processes)
		if !e.CountProcessState && (active != pool.ActiveProcesses || idle != pool.IdleProcesses) {
			log.Error("Inconsistent active and idle processes reported. Set `--phpfpm.fix-process-count` to have this calculated by php-fpm_exporter instead.")
		}

		if !e.CountProcessState {
			active = pool.ActiveProcesses
			idle = pool.IdleProcesses
			total = pool.TotalProcesses
		}

		ch <- prometheus.MustNewConstMetric(e.up, prometheus.GaugeValue, 1, poolValues...)
		ch <- prometheus.MustNewConstMetric(e.startSince, prometheus.CounterValue, float64(pool.StartSince), poolValues...)
		ch <- prometheus.MustNewConstMetric(e.acceptedConnections, prometheus.CounterValue, float64(pool.AcceptedConnections), poolValues...)
		ch <- prometheus.MustNewConstMetric(e.listenQueue, prometheus.GaugeValue, float64(pool.ListenQueue), poolValues...)
		ch <- prometheus.MustNewConstMetric(e.maxListenQueue, prometheus.CounterValue, float64(pool.MaxListenQueue), poolValues...)
		ch <- prometheus.MustNewConstMetric(e.listenQueueLength, prometheus.GaugeValue, float64(pool.ListenQueueLength), poolValues...)
		ch <- prometheus.MustNewConstMetric(e.idleProcesses, prometheus.GaugeValue, float64(idle), poolValues...)
		ch <- prometheus.MustNewConstMetric(e.activeProcesses, prometheus.GaugeValue, float64(active), poolValues...)
		ch <- prometheus.MustNewConstMetric(e.totalProcesses, prometheus.GaugeValue, float64(total), poolValues...)
		ch <- prometheus.MustNewConstMetric(e.maxActiveProcesses, prometheus.CounterValue, float64(pool.MaxActiveProcesses), poolValues...)
		ch <- prometheus.MustNewConstMetric(e.maxChildrenReached, prometheus.CounterValue, float64(pool.MaxChildrenReached), poolValues...)
		ch <- prometheus.MustNewConstMetric(e.slowRequests, prometheus.CounterValue, float64(pool.SlowRequests), poolValues...)

		for childNumber, process := range pool.Processes {
			childName := strconv.Itoa(childNumber)
			processValues := e.processLabelValues(pool, childName)

			states := map[string]int{
				PoolProcessRequestIdle:           0,
				PoolProcessRequestRunning:        0,
				PoolProcessRequestFinishing:      0,
				PoolProcessRequestReadingHeaders: 0,
				PoolProcessRequestInfo:           0,
				PoolProcessRequestEnding:         0,
			}
			states[process.State]++

			for stateName, inState := range states {
				ch <- prometheus.MustNewConstMetric(e.processState, prometheus.GaugeValue, float64(inState), e.processStateLabelValues(pool, childName, stateName)...)
			}
			ch <- prometheus.MustNewConstMetric(e.processRequests, prometheus.CounterValue, float64(process.Requests), processValues...)
			ch <- prometheus.MustNewConstMetric(e.processLastRequestMemory, prometheus.GaugeValue, float64(process.LastRequestMemory), processValues...)
			ch <- prometheus.MustNewConstMetric(e.processLastRequestCPU, prometheus.GaugeValue, process.LastRequestCPU, processValues...)
			ch <- prometheus.MustNewConstMetric(e.processRequestDuration, prometheus.GaugeValue, float64(process.RequestDuration), processValues...)
		}
	}
}

// Describe exposes the metric description to Prometheus
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.up
	ch <- e.scrapeFailues
	ch <- e.startSince
	ch <- e.acceptedConnections
	ch <- e.listenQueue
	ch <- e.maxListenQueue
	ch <- e.listenQueueLength
	ch <- e.idleProcesses
	ch <- e.activeProcesses
	ch <- e.totalProcesses
	ch <- e.maxActiveProcesses
	ch <- e.maxChildrenReached
	ch <- e.slowRequests
	ch <- e.processState
	ch <- e.processRequests
	ch <- e.processLastRequestMemory
	ch <- e.processLastRequestCPU
	ch <- e.processRequestDuration
}

// UpdatePoolManager updates the Pool Manager
func (e *Exporter) UpdatePoolManager(newPM PoolManager) {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	e.PoolManager = newPM
	log.Info("PoolManager has been updated")
}
