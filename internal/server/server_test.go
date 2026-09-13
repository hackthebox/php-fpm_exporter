// Copyright 2026 Hack The Box
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hackthebox/php-fpm_exporter/phpfpm"
)

func TestMain(m *testing.M) {
	// The phpfpm package logs through a package-global that is nil until set.
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	phpfpm.SetLogger(logger)

	os.Exit(m.Run())
}

func testConfig(t *testing.T) Config {
	t.Helper()

	return Config{
		MetricsEndpoint: "/metrics",
		// Nothing listens on port 1, so a scrape fails immediately instead of
		// waiting out the timeout.
		ScrapeURIs:      []string{"tcp://127.0.0.1:1/status"},
		ScrapeTimeout:   50 * time.Millisecond,
		ShutdownTimeout: 5 * time.Second,
		Registry:        prometheus.NewRegistry(),
	}
}

// serveInBackground runs the server on an ephemeral port and returns its base URL.
func serveInBackground(ctx context.Context, t *testing.T, cfg Config) (string, <-chan error) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	errs := make(chan error, 1)
	go func() { errs <- serve(ctx, cfg, listener) }()

	return "http://" + listener.Addr().String(), errs
}

func fetch(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()

	resp, err := fetch(t.Context(), url)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp.StatusCode, string(body)
}

// A zero Config must be usable: production leaves these unset and relies on the
// prometheus default registry, which is what carries the go_* and process_*
// series.
func TestConfigDefaultsToTheGlobals(t *testing.T) {
	var zero Config

	assert.Same(t, prometheus.DefaultRegisterer, zero.registerer())
	assert.Same(t, prometheus.DefaultGatherer, zero.gatherer())
	assert.NotNil(t, zero.logger(), "a nil logger must not panic on first use")
	assert.Equal(t, io.Discard, zero.logger().Out, "an unset logger discards rather than writing to stderr")
}

func TestConfigUsesTheSuppliedRegistryAndLogger(t *testing.T) {
	registry := prometheus.NewRegistry()
	logger := logrus.New()

	cfg := Config{Registry: registry, Logger: logger}

	assert.Same(t, registry, cfg.registerer())
	assert.Same(t, registry, cfg.gatherer())
	assert.Same(t, logger, cfg.logger())
}

// failingWriter is a ResponseWriter whose body writes always fail, which is the
// only way to reach the landing page's error branch.
type failingWriter struct{ http.ResponseWriter }

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestNewMuxSurvivesAFailedLandingPageWrite(t *testing.T) {
	logger, hook := logrustest.NewNullLogger()

	cfg := testConfig(t)
	cfg.Logger = logger

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	newMux(cfg).ServeHTTP(failingWriter{httptest.NewRecorder()}, req)

	require.Len(t, hook.Entries, 1, "a failed write must be logged, not swallowed")
	assert.Equal(t, logrus.ErrorLevel, hook.LastEntry().Level)
	assert.Contains(t, hook.LastEntry().Message, "connection reset")
}

func TestServeExposesExporterMetricsOnTheConfiguredPath(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	cfg := testConfig(t)
	cfg.MetricsEndpoint = "/custom-metrics"

	baseURL, errs := serveInBackground(ctx, t, cfg)

	status, body := get(t, baseURL+"/custom-metrics")

	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, "phpfpm_up", "the exporter must be registered with the served registry")
	assert.Contains(t, body, `scrape_uri="tcp://127.0.0.1:1/status"`, "static scrape URIs become pools")

	// "/" is a catch-all, so another path gets the landing page rather than 404.
	status, body = get(t, baseURL+"/metrics")
	assert.Equal(t, http.StatusOK, status)
	assert.NotContains(t, body, "phpfpm_up", "only the configured path serves metrics")

	cancel()
	require.NoError(t, <-errs)
}

func TestServeLinksToMetricsFromTheRoot(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	cfg := testConfig(t)
	cfg.MetricsEndpoint = "/custom-metrics"

	baseURL, errs := serveInBackground(ctx, t, cfg)

	status, body := get(t, baseURL+"/")

	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `href='/custom-metrics'`, "the landing page points at the configured path")

	cancel()
	require.NoError(t, <-errs)
}

func TestServeShutsDownWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	baseURL, errs := serveInBackground(ctx, t, testConfig(t))

	status, _ := get(t, baseURL+"/metrics")
	require.Equal(t, http.StatusOK, status)

	cancel()

	select {
	case err := <-errs:
		require.NoError(t, err, "a clean shutdown is not an error")
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling the context must shut the server down")
	}

	_, err := fetch(t.Context(), baseURL+"/metrics")
	assert.Error(t, err, "the listener must be closed after shutdown")
}

func TestServeReportsAListenerFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, listener.Close())

	err = serve(t.Context(), testConfig(t), listener)

	require.Error(t, err, "a dead listener must surface, not leave the process running serverless")
	assert.NotErrorIs(t, err, http.ErrServerClosed)
}

func TestRunReportsAnUnbindableAddress(t *testing.T) {
	cfg := testConfig(t)
	cfg.ListenAddress = "127.0.0.1:99999"

	err := Run(t.Context(), cfg)

	require.Error(t, err, "a bind failure must surface rather than be logged and ignored")
	assert.Contains(t, err.Error(), "99999")
}

func TestRunServesOnTheConfiguredAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	cfg := testConfig(t)
	cfg.ListenAddress = address

	errs := make(chan error, 1)
	go func() { errs <- Run(ctx, cfg) }()

	require.Eventually(t, func() bool {
		resp, err := fetch(ctx, "http://"+address+"/metrics")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 10*time.Second, 20*time.Millisecond, "Run must bind the configured address")

	cancel()
	require.NoError(t, <-errs)
}

func TestNewExporterCountsProcessStateWhenConfigured(t *testing.T) {
	tests := []struct {
		name string
		fix  bool
	}{
		{name: "php-fpm reports the counts", fix: false},
		{name: "the exporter recounts", fix: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.FixProcessCount = tt.fix

			exporter := newExporter(t.Context(), cfg)

			assert.Equal(t, tt.fix, exporter.CountProcessState)
		})
	}
}

func TestNewExporterAddsEveryStaticScrapeURI(t *testing.T) {
	cfg := testConfig(t)
	cfg.ScrapeURIs = []string{
		"tcp://127.0.0.1:1/status",
		"tcp://127.0.0.2:1/status",
	}

	exporter := newExporter(t.Context(), cfg)

	require.Len(t, exporter.PoolManager.Pools, 2, "the exporter must see the static pools")
	assert.Equal(t, "tcp://127.0.0.1:1/status", exporter.PoolManager.Pools[0].Address)
	assert.Equal(t, "tcp://127.0.0.2:1/status", exporter.PoolManager.Pools[1].Address)
	assert.Empty(t, exporter.PoolManager.Pools[0].Pod, "statically configured pools carry no pod name")
	assert.Equal(t, cfg.ScrapeTimeout, exporter.PoolManager.ScrapeTimeout)
}

// The two discovery modes are mutually exclusive: with auto-tracking on, the
// static URIs must not be scraped as well. Discovery itself needs in-cluster
// config, so outside a pod it fails in the background and the exporter stays
// empty rather than falling back to the wrong targets.
func TestNewExporterIgnoresStaticURIsWhenAutoTracking(t *testing.T) {
	cfg := testConfig(t)
	cfg.K8sAutoTracking = true

	exporter := newExporter(t.Context(), cfg)

	assert.Empty(t, exporter.PoolManager.Pools)
	assert.NotNil(t, exporter.PoolManager.PodPhases, "discovery needs the phase map initialised")
}
