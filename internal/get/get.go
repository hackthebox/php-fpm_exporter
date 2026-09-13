// Copyright 2026 Hack The Box
// SPDX-License-Identifier: Apache-2.0

// Package get renders PHP-FPM pool statistics in one of the supported output
// formats. It holds what used to live in the cobra command body, so that cmd/
// stays flag parsing and wiring.
package get

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/gosuri/uitable"

	"github.com/hackthebox/php-fpm_exporter/phpfpm"
)

// maxColWidth caps a rendered cell; uitable elides anything longer.
const maxColWidth = 80

// Config is the get command's settings, one field per CLI flag.
type Config struct {
	ScrapeURIs    []string
	ScrapeTimeout time.Duration
	Output        string
}

// Run scrapes every configured pool and writes the result to w in the requested
// format. A scrape failure is returned but does not suppress the output: a
// partial result is still worth having, and the caller decides what to do about
// the error.
func Run(cfg Config, w io.Writer) error {
	pm := phpfpm.PoolManager{ScrapeTimeout: cfg.ScrapeTimeout}

	for _, uri := range cfg.ScrapeURIs {
		pm.Add(uri, "")
	}

	scrapeErr := pm.Update()

	if err := write(pm, cfg.Output, w); err != nil {
		return err
	}

	return scrapeErr
}

func write(pm phpfpm.PoolManager, output string, w io.Writer) error {
	switch output {
	case "json":
		return json.NewEncoder(w).Encode(pm)
	case "text":
		_, err := fmt.Fprintln(w, table(pm))
		return err
	case "spew":
		spew.Fdump(w, pm)
		return nil
	default:
		return fmt.Errorf("output format %q is not valid, expected one of: text, json, spew", output)
	}
}

func table(pm phpfpm.PoolManager) *uitable.Table {
	t := uitable.New()
	t.MaxColWidth = maxColWidth

	for _, pool := range pm.Pools {
		t.AddRow("Address:", pool.Address)
		t.AddRow("Pool:", pool.Name)
		t.AddRow("Start time:", time.Time(pool.StartTime).Format(time.RFC1123Z))
		t.AddRow("Start since:", pool.StartSince)
		t.AddRow("Accepted connections:", pool.AcceptedConnections)
		t.AddRow("Listen Queue:", pool.ListenQueue)
		t.AddRow("Max Listen Queue:", pool.MaxListenQueue)
		t.AddRow("Listen Queue Length:", pool.ListenQueueLength)
		t.AddRow("Idle Processes:", pool.IdleProcesses)
		t.AddRow("Active Processes:", pool.ActiveProcesses)
		t.AddRow("Total Processes:", pool.TotalProcesses)
		t.AddRow("Max active processes:", pool.MaxActiveProcesses)
		t.AddRow("Max children reached:", pool.MaxChildrenReached)
		t.AddRow("Slow requests:", pool.SlowRequests)
		t.AddRow("")
	}

	return t
}
