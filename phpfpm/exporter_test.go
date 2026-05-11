package phpfpm

import (
	"strings"
	"testing"

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
	exporter := NewExporter(PoolManager{})

	assert.Contains(t, exporter.up.String(), "variableLabels: {pool,pod,scrape_uri}")
	assert.Contains(t, exporter.scrapeFailues.String(), "variableLabels: {pool,pod,scrape_uri}")
	assert.Contains(t, exporter.processRequests.String(), "variableLabels: {pool,pod,child,scrape_uri}")
	assert.Contains(t, exporter.processState.String(), "variableLabels: {pool,pod,child,state,scrape_uri}")
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
		{name: "static target", pod: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exporter := NewExporter(PoolManager{})
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
				assertMetricLabelValue(t, dtoMetric.GetLabel(), "pod", tt.pod)
			}

			assert.NotZero(t, metricCount)
		})
	}
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
