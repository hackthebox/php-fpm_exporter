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
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	namespace      = "phpfpm"
	phpfpmPodLabel = "phpfpm_pod"
)

var (
	poolMetricLabels         = []string{"pool", phpfpmPodLabel, "scrape_uri"}
	processMetricLabels      = []string{"pool", phpfpmPodLabel, "child", "scrape_uri"}
	processStateMetricLabels = []string{"pool", phpfpmPodLabel, "child", "state", "scrape_uri"}
)

func poolLabelValues(pool Pool) []string {
	return []string{pool.Name, pool.Pod, pool.Address}
}

func processLabelValues(pool Pool, child string) []string {
	return []string{pool.Name, pool.Pod, child, pool.Address}
}

func processStateLabelValues(pool Pool, child string, state string) []string {
	return []string{pool.Name, pool.Pod, child, state, pool.Address}
}

// Exporter configures and exposes PHP-FPM metrics to Prometheus.
type Exporter struct {
	mutex       sync.Mutex
	PoolManager PoolManager

	CountProcessState bool

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
func NewExporter(pm PoolManager) *Exporter {
	return &Exporter{
		PoolManager: pm,

		CountProcessState: false,

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

	if err := e.PoolManager.Update(); err != nil {
		log.Error(err)
	}

	e.collectPools(ch, e.PoolManager.Pools)
}

func (e *Exporter) collectPools(ch chan<- prometheus.Metric, pools []Pool) {
	for _, pool := range pools {
		poolValues := poolLabelValues(pool)

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
			childName := fmt.Sprintf("%d", childNumber)
			processValues := processLabelValues(pool, childName)

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
				ch <- prometheus.MustNewConstMetric(e.processState, prometheus.GaugeValue, float64(inState), processStateLabelValues(pool, childName, stateName)...)
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
