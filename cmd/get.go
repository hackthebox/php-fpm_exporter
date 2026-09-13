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
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/hackthebox/php-fpm_exporter/internal/get"
)

// Configuration variables
var (
	output string
)

// getCmd represents the get command
var getCmd = &cobra.Command{
	Use:   "get",
	Short: "Returns metrics without running as a server",
	Long: `"get" fetches metrics from php-fpm. Multiple addresses can be specified as follows:

* php-fpm_exporter get --phpfpm.scrape-uri 127.0.0.1:9000 --phpfpm.scrape-uri 127.0.0.1:9001 [...]
* php-fpm_exporter get --phpfpm.scrape-uri 127.0.0.1:9000,127.0.0.1:9001,[...]
`,
	Run: func(cmd *cobra.Command, args []string) {
		err := get.Run(get.Config{
			ScrapeURIs:    scrapeURIs,
			ScrapeTimeout: scrapeTimeout,
			Output:        output,
		}, os.Stdout)
		if err != nil {
			log.Error(err)
			os.Exit(1)
		}
	},
}

func init() {
	RootCmd.AddCommand(getCmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// getCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	getCmd.Flags().StringSliceVar(&scrapeURIs, "phpfpm.scrape-uri", []string{"tcp://127.0.0.1:9000/status"}, "FastCGI address, e.g. unix:///tmp/php.sock;/status or tcp://127.0.0.1:9000/status")
	getCmd.Flags().DurationVar(&scrapeTimeout, "phpfpm.scrape-timeout", 3*time.Second, "Timeout for scraping a PHP-FPM status endpoint; a stalled endpoint aborts after this instead of blocking.")
	getCmd.Flags().StringVar(&output, "out", "text", "Output format. One of: text, json, spew")
}
