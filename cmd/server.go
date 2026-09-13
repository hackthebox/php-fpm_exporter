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

package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/hackthebox/php-fpm_exporter/internal/server"
)

// Configuration variables
var (
	listeningAddress string
	metricsEndpoint  string
	scrapeURIs       []string
	fixProcessCount  bool
	scrapeTimeout    time.Duration
	k8sAutoTracking  bool
	namespace        string
	podLabels        string
	port             string
)

// serverCmd represents the server command
var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "A brief description of your command",
	Long: `A longer description that spans multiple lines and likely contains examples
and usage of using your command. For example:

Cobra is a CLI library for Go that empowers applications.
This application is a tool to generate the needed files
to quickly create a Cobra application.`,
	Run: func(cmd *cobra.Command, args []string) {
		log.Infof("Starting server on %v with path %v", listeningAddress, metricsEndpoint)

		// We'll accept graceful shutdowns when quit via SIGINT (Ctrl+C) or SIGTERM
		// SIGKILL, SIGQUIT will not be caught.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		err := server.Run(ctx, server.Config{
			ListenAddress:   listeningAddress,
			MetricsEndpoint: metricsEndpoint,
			ScrapeURIs:      scrapeURIs,
			ScrapeTimeout:   scrapeTimeout,
			FixProcessCount: fixProcessCount,
			K8sAutoTracking: k8sAutoTracking,
			Namespace:       namespace,
			PodLabels:       podLabels,
			Port:            port,
			ShutdownTimeout: 10 * time.Second,
			Logger:          log,
		})
		if err != nil {
			log.Fatal("Error running server: ", err)
		}

		log.Info("Shutting down")
	},
}

func init() {
	RootCmd.AddCommand(serverCmd)

	// Web
	serverCmd.Flags().StringVar(&listeningAddress, "web.listen-address", ":9253", "Address on which to expose metrics and web interface.")
	serverCmd.Flags().StringVar(&metricsEndpoint, "web.telemetry-path", "/metrics", "Path under which to expose metrics.")

	// PHP FPM
	serverCmd.Flags().StringSliceVar(&scrapeURIs, "phpfpm.scrape-uri", []string{"tcp://127.0.0.1:9000/status"}, "FastCGI address, e.g. unix:///tmp/php.sock;/status or tcp://127.0.0.1:9000/status")
	serverCmd.Flags().BoolVar(&fixProcessCount, "phpfpm.fix-process-count", false, "Enable to calculate process numbers via php-fpm_exporter since PHP-FPM sporadically reports wrong active/idle/total process numbers.")
	serverCmd.Flags().DurationVar(&scrapeTimeout, "phpfpm.scrape-timeout", 3*time.Second, "Timeout for scraping a PHP-FPM status endpoint; a stalled endpoint aborts after this instead of blocking.")

	// Kubernetes
	serverCmd.Flags().BoolVar(&k8sAutoTracking, "k8s.autotracking", false, "Enable automatic tracking of PHP-FPM pods in Kubernetes.")
	serverCmd.Flags().StringVarP(&namespace, "k8s.namespace", "n", "", "Kubernetes namespace to monitor (defaults to all namespaces if not set)")
	serverCmd.Flags().StringVarP(&podLabels, "k8s.pod-labels", "l", "php-fpm-exporter/collect=true", "Kubernetes pod labels as a list of key-value pairs")
	serverCmd.Flags().StringVarP(&port, "k8s.port", "p", "9000", "Kubernetes pod port")

	// Workaround since vipers BindEnv is currently not working as expected (see https://github.com/spf13/viper/issues/461)

	envs := map[string]string{
		"PHP_FPM_WEB_LISTEN_ADDRESS": "web.listen-address",
		"PHP_FPM_WEB_TELEMETRY_PATH": "web.telemetry-path",
		"PHP_FPM_SCRAPE_URI":         "phpfpm.scrape-uri",
		"PHP_FPM_FIX_PROCESS_COUNT":  "phpfpm.fix-process-count",
		"PHP_FPM_SCRAPE_TIMEOUT":     "phpfpm.scrape-timeout",
		"PHP_FPM_K8S_AUTOTRACKING":   "k8s.autotracking",
		"PHP_FPM_K8S_NAMESPACE":      "k8s.namespace",
		"PHP_FPM_K8S_POD_LABELS":     "k8s.pod-labels",
		"PHP_FPM_K8S_POD_PORT":       "k8s.port",
	}

	mapEnvVars(envs, serverCmd)
}
