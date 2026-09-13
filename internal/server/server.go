// Copyright 2026 Hack The Box
// SPDX-License-Identifier: Apache-2.0

// Package server runs the Prometheus endpoint. It holds what used to live in
// the cobra command body, so that cmd/ stays flag parsing and wiring.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"

	"github.com/hackthebox/php-fpm_exporter/phpfpm"
)

// Config is the server's settings, one field per CLI flag.
type Config struct {
	ListenAddress   string
	MetricsEndpoint string
	ScrapeURIs      []string
	ScrapeTimeout   time.Duration
	FixProcessCount bool
	K8sAutoTracking bool
	Namespace       string
	PodLabels       string
	Port            string
	ShutdownTimeout time.Duration

	// Registry defaults to the prometheus default registry, which is what puts
	// the go_* and process_* series on /metrics. Tests pass their own so that
	// repeated runs do not collide on the global.
	Registry *prometheus.Registry

	// Logger defaults to discarding output, so a zero Config is usable.
	Logger *logrus.Logger
}

func (c Config) logger() *logrus.Logger {
	if c.Logger == nil {
		discard := logrus.New()
		discard.SetOutput(io.Discard)
		return discard
	}
	return c.Logger
}

func (c Config) registerer() prometheus.Registerer {
	if c.Registry == nil {
		return prometheus.DefaultRegisterer
	}
	return c.Registry
}

func (c Config) gatherer() prometheus.Gatherer {
	if c.Registry == nil {
		return prometheus.DefaultGatherer
	}
	return c.Registry
}

// Run serves metrics on cfg.ListenAddress until ctx is cancelled, then shuts
// down gracefully within cfg.ShutdownTimeout.
func Run(ctx context.Context, cfg Config) error {
	listener, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", cfg.ListenAddress, err)
	}

	return serve(ctx, cfg, listener)
}

func serve(ctx context.Context, cfg Config, listener net.Listener) error {
	exporter := newExporter(ctx, cfg)
	cfg.registerer().MustRegister(exporter)

	srv := &http.Server{
		Handler: newMux(cfg),
		// Good practice to set timeouts to avoid Slowloris attacks.
		WriteTimeout: time.Second * 15,
		ReadTimeout:  time.Second * 15,
		IdleTimeout:  time.Second * 60,
	}

	// A failed Serve used to be logged and swallowed, leaving the process alive
	// and blocked on the signal with nothing listening.
	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(listener) }()

	select {
	case err := <-failed:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	// Doesn't block if no connections, but will otherwise wait until the
	// timeout deadline.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	return srv.Shutdown(shutdownCtx)
}

func newExporter(ctx context.Context, cfg Config) *phpfpm.Exporter {
	pm := phpfpm.PoolManager{
		PodPhases:     make(map[string]v1.PodPhase),
		ScrapeTimeout: cfg.ScrapeTimeout,
	}

	// Initialize the Exporter before any dynamic or static setup
	exporter := phpfpm.NewExporter(pm)
	exporter.CountProcessState = cfg.FixProcessCount

	if cfg.FixProcessCount {
		cfg.logger().Info("Idle/Active/Total Processes will be calculated by php-fpm_exporter.")
	}

	if cfg.K8sAutoTracking {
		cfg.logger().Info("Kubernetes auto-tracking enabled. Watching for pod changes...")

		go func() {
			if err := pm.DiscoverPods(ctx, exporter, cfg.Namespace, cfg.PodLabels, cfg.Port); err != nil {
				cfg.logger().Error(err)
			}
		}()

		return exporter
	}

	// Static scraping of predefined URIs
	for _, uri := range cfg.ScrapeURIs {
		pm.Add(uri, "")
	}
	exporter.UpdatePoolManager(pm)

	return exporter
}

func newMux(cfg Config) *http.ServeMux {
	mux := http.NewServeMux()

	// InstrumentMetricHandler is what promhttp.Handler() wraps its own handler
	// in; without it the promhttp_metric_handler_* series vanish from /metrics.
	mux.Handle(cfg.MetricsEndpoint, promhttp.InstrumentMetricHandler(
		cfg.registerer(),
		promhttp.HandlerFor(cfg.gatherer(), promhttp.HandlerOpts{}),
	))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, err := w.Write([]byte(`<html>
			 <head><title>php-fpm_exporter</title></head>
			 <body>
			 <h1>php-fpm_exporter</h1>
			 <p><a href='` + cfg.MetricsEndpoint + `'>Metrics</a></p>
			 </body>
			 </html>`))
		if err != nil {
			cfg.logger().Error(err)
		}
	})

	return mux
}
