package phpfpm

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPoolManagerAddStoresPodName(t *testing.T) {
	pm := PoolManager{}

	pool := pm.Add("tcp://10.0.0.1:9000/status", "php-fpm-0")

	assert.Equal(t, "php-fpm-0", pool.Pod)
	require.Len(t, pm.Pools, 1)
	assert.Equal(t, "php-fpm-0", pm.Pools[0].Pod)
}

func TestNewExporterDescriptorsIncludePodLabel(t *testing.T) {
	exporter := NewExporter(PoolManager{}, WithPodLabel())

	assert.Contains(t, exporter.up.String(), "variableLabels: {pool,phpfpm_pod,scrape_uri}")
	assert.Contains(t, exporter.scrapeFailues.String(), "variableLabels: {pool,phpfpm_pod,scrape_uri}")
	assert.Contains(t, exporter.processRequests.String(), "variableLabels: {pool,phpfpm_pod,child,scrape_uri}")
	assert.Contains(t, exporter.processState.String(), "variableLabels: {pool,phpfpm_pod,child,state,scrape_uri}")
}

// Only Kubernetes auto-tracking can populate phpfpm_pod. A static target has no
// pod name, and Prometheus treats an empty label value as equivalent to the
// label being absent, so emitting it is noise. It cannot be dropped per pool:
// client_golang rejects two descriptors sharing a metric name with differing
// label names, so the choice is made once per exporter.
func TestNewExporterOmitsPodLabelByDefault(t *testing.T) {
	exporter := NewExporter(PoolManager{})

	assert.Contains(t, exporter.up.String(), "variableLabels: {pool,scrape_uri}")
	assert.Contains(t, exporter.scrapeFailues.String(), "variableLabels: {pool,scrape_uri}")
	assert.Contains(t, exporter.processRequests.String(), "variableLabels: {pool,child,scrape_uri}")
	assert.Contains(t, exporter.processState.String(), "variableLabels: {pool,child,state,scrape_uri}")
}

func TestExporterCollectPoolsOmitsPodLabelForStaticTargets(t *testing.T) {
	exporter := NewExporter(PoolManager{})
	exporter.CountProcessState = true

	ch := make(chan prometheus.Metric, 64)
	exporter.collectPools(ch, []Pool{{
		Address:   "tcp://127.0.0.1:9000/status",
		Name:      "www",
		Processes: []PoolProcess{{State: PoolProcessRequestIdle}},
	}})
	close(ch)

	metricCount := 0

	for metric := range ch {
		metricCount++

		dtoMetric := &dto.Metric{}
		require.NoError(t, metric.Write(dtoMetric))

		for _, label := range dtoMetric.GetLabel() {
			assert.NotEqual(t, labelPod, label.GetName(), "static series must carry no pod label at all")
		}
	}

	assert.NotZero(t, metricCount)
}

// The registry rejects a collector whose metrics disagree with its descriptors,
// so gathering proves the label set and the values actually line up.
func TestExporterGathersInBothModes(t *testing.T) {
	tests := []struct {
		name     string
		opts     []Option
		wantPod  bool
		podValue string
	}{
		{name: "static", opts: nil, wantPod: false},
		{name: "kubernetes", opts: []Option{WithPodLabel()}, wantPod: true, podValue: "php-fpm-0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pm := PoolManager{}
			pm.Add("tcp://127.0.0.1:9000/status", tt.podValue)

			exporter := NewExporter(pm, tt.opts...)
			exporter.CountProcessState = true

			registry := prometheus.NewRegistry()
			require.NoError(t, registry.Register(exporter))

			families, err := registry.Gather()
			require.NoError(t, err, "the registry must accept the descriptors and metrics together")
			require.NotEmpty(t, families)

			found := false

			for _, family := range families {
				for _, metric := range family.GetMetric() {
					for _, label := range metric.GetLabel() {
						if label.GetName() == labelPod {
							found = true
							assert.Equal(t, tt.podValue, label.GetValue())
						}
					}
				}
			}

			assert.Equal(t, tt.wantPod, found)
		})
	}
}

func TestExporterDescribeIncludesScrapeFailuresDescriptor(t *testing.T) {
	exporter := NewExporter(PoolManager{})
	descs := make(chan *prometheus.Desc, 32)

	exporter.Describe(descs)
	close(descs)

	foundScrapeFailures := false
	descCount := 0

	for desc := range descs {
		descCount++
		if strings.Contains(desc.String(), "phpfpm_scrape_failures") {
			foundScrapeFailures = true
		}
	}

	assert.True(t, foundScrapeFailures)
	assert.Equal(t, 18, descCount)
}

func TestExporterCollectPoolsIncludesPodLabel(t *testing.T) {
	tests := []struct {
		name string
		pod  string
	}{
		{name: "kubernetes discovered target", pod: "php-fpm-0"},
		{name: "pod not yet named", pod: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exporter := NewExporter(PoolManager{}, WithPodLabel())
			exporter.CountProcessState = true

			ch := make(chan prometheus.Metric, 64)
			exporter.collectPools(ch, []Pool{
				{
					Address:             "tcp://10.0.0.1:9000/status",
					Pod:                 tt.pod,
					Name:                "www",
					StartSince:          1,
					AcceptedConnections: 2,
					ListenQueueLength:   128,
					MaxActiveProcesses:  1,
					Processes: []PoolProcess{
						{
							State:             PoolProcessRequestRunning,
							Requests:          3,
							RequestDuration:   10,
							LastRequestCPU:    12.5,
							LastRequestMemory: 2048,
						},
					},
				},
			})
			close(ch)

			metricCount := 0

			for metric := range ch {
				metricCount++

				dtoMetric := &dto.Metric{}
				require.NoError(t, metric.Write(dtoMetric))
				assertMetricLabelValue(t, dtoMetric.GetLabel(), labelPod, tt.pod)
			}

			assert.NotZero(t, metricCount)
		})
	}
}

func TestExporterCollectPoolsNumbersChildrenByIndex(t *testing.T) {
	exporter := NewExporter(PoolManager{})
	exporter.CountProcessState = true

	ch := make(chan prometheus.Metric, 64)
	exporter.collectPools(ch, []Pool{
		{
			Address: "tcp://10.0.0.1:9000/status",
			Name:    "www",
			Processes: []PoolProcess{
				{State: PoolProcessRequestIdle},
				{State: PoolProcessRequestRunning},
				{State: PoolProcessRequestIdle},
			},
		},
	})
	close(ch)

	seen := map[string]bool{}

	for metric := range ch {
		dtoMetric := &dto.Metric{}
		require.NoError(t, metric.Write(dtoMetric))

		for _, label := range dtoMetric.GetLabel() {
			if label.GetName() == labelChild {
				seen[label.GetValue()] = true
			}
		}
	}

	assert.Equal(t, map[string]bool{"0": true, "1": true, "2": true}, seen, "child is the decimal slice index")
}

func assertMetricLabelValue(t *testing.T, labels []*dto.LabelPair, name string, want string) {
	t.Helper()

	for _, label := range labels {
		if label.GetName() == name {
			assert.Equal(t, want, label.GetValue())
			return
		}
	}

	t.Fatalf("label %q not found", name)
}

// Pool.error logs each failure, so the joined error must not be logged again:
// a dead target would otherwise produce two identical lines per scrape.
func TestExporterCollectLogsEachScrapeFailureOnce(t *testing.T) {
	logs := captureLogs(t)

	pm := PoolManager{ScrapeTimeout: 50 * time.Millisecond}
	pm.Add("tcp://127.0.0.1:1/status", "")
	exporter := NewExporter(pm)

	ch := make(chan prometheus.Metric, 64)
	go func() {
		exporter.Collect(ch)
		close(ch)
	}()
	for range ch { //nolint:revive // draining is the point
	}

	close(logs)

	refused := 0
	for msg := range logs {
		if strings.Contains(msg, "connection refused") {
			refused++
			assert.Contains(t, msg, "Error scraping PHP-FPM",
				"the surviving line is the one with context")
		}
	}

	assert.Equal(t, 1, refused, "one line per failing pool, not one per layer that saw the error")
}
