// Copyright 2026 Hack The Box
// SPDX-License-Identifier: Apache-2.0

package get

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hackthebox/php-fpm_exporter/phpfpm"
)

func TestMain(m *testing.M) {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	phpfpm.SetLogger(logger)

	os.Exit(m.Run())
}

// Nothing listens on port 1, so a scrape fails immediately rather than waiting
// out the timeout. Every pool therefore reports its zero values, which is enough
// to assert how each format renders.
func testConfig() Config {
	return Config{
		ScrapeURIs:    []string{"tcp://127.0.0.1:1/status"},
		ScrapeTimeout: 50 * time.Millisecond,
		Output:        "json",
	}
}

func TestRunWritesJSONToTheSuppliedWriter(t *testing.T) {
	cfg := testConfig()
	cfg.Output = "json"

	var out bytes.Buffer
	err := Run(cfg, &out)

	require.Error(t, err, "the target is unreachable, so the scrape must be reported")

	var decoded phpfpm.PoolManager
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded), "output must be valid JSON: %s", out.String())
	require.Len(t, decoded.Pools, 1)
}

func TestRunWritesTheTextTable(t *testing.T) {
	cfg := testConfig()
	cfg.Output = "text"

	var out bytes.Buffer
	_ = Run(cfg, &out)

	body := out.String()
	for _, want := range []string{
		"Address:", "Pool:", "Start time:", "Accepted connections:",
		"Idle Processes:", "Active Processes:", "Total Processes:", "Slow requests:",
	} {
		assert.Contains(t, body, want)
	}
	assert.Contains(t, body, "tcp://127.0.0.1:1/status", "the table names the target")
}

// Every field gets a distinct value, so dropping any single row is visible.
func TestTableRendersEveryPoolField(t *testing.T) {
	pm := phpfpm.PoolManager{Pools: []phpfpm.Pool{{
		Address:             "tcp://10.0.0.1:9000/status",
		Name:                "example-pool",
		StartSince:          111,
		AcceptedConnections: 222,
		ListenQueue:         333,
		MaxListenQueue:      444,
		ListenQueueLength:   555,
		IdleProcesses:       666,
		ActiveProcesses:     777,
		TotalProcesses:      888,
		MaxActiveProcesses:  999,
		MaxChildrenReached:  1010,
		SlowRequests:        1111,
	}}}

	got := table(pm).String()

	rows := [][2]string{
		{"Address:", "tcp://10.0.0.1:9000/status"},
		{"Pool:", "example-pool"},
		{"Start time:", "Mon, 01 Jan 0001 00:00:00 +0000"},
		{"Start since:", "111"},
		{"Accepted connections:", "222"},
		{"Listen Queue:", "333"},
		{"Max Listen Queue:", "444"},
		{"Listen Queue Length:", "555"},
		{"Idle Processes:", "666"},
		{"Active Processes:", "777"},
		{"Total Processes:", "888"},
		{"Max active processes:", "999"},
		{"Max children reached:", "1010"},
		{"Slow requests:", "1111"},
	}

	for _, row := range rows {
		assert.Contains(t, got, row[0], "missing label")
		assert.Contains(t, got, row[1], "missing value for %s", row[0])
	}
}

func TestTableSeparatesConsecutivePools(t *testing.T) {
	pm := phpfpm.PoolManager{Pools: []phpfpm.Pool{
		{Address: "tcp://10.0.0.1:9000/status", Name: "first"},
		{Address: "tcp://10.0.0.2:9000/status", Name: "second"},
	}}

	lines := strings.Split(table(pm).String(), "\n")

	first, second := -1, -1
	for i, line := range lines {
		switch {
		case strings.Contains(line, "10.0.0.1"):
			first = i
		case strings.Contains(line, "10.0.0.2"):
			second = i
		}
	}

	require.NotEqual(t, -1, first, "the first pool must be rendered")
	require.Greater(t, second, first, "the second pool must follow the first")

	separated := false
	for _, line := range lines[first+1 : second] {
		if strings.TrimSpace(line) == "" {
			separated = true
		}
	}

	assert.True(t, separated, "a blank row must separate one pool from the next")
}

// MaxColWidth caps a cell at 80 columns and uitable elides the remainder, so a
// scrape URI longer than that is shown truncated. Worth knowing before reading a
// long unix socket path out of `get --out text`.
func TestTableTruncatesOverlongValues(t *testing.T) {
	long := "tcp://" + strings.Repeat("a", 120) + ":9000/status"
	pm := phpfpm.PoolManager{Pools: []phpfpm.Pool{{Address: long, Name: "www"}}}

	got := table(pm).String()

	assert.NotContains(t, got, long, "an overlong value is not printed in full")
	assert.Contains(t, got, "...", "the elision is what tells the reader it was cut")

	for _, line := range strings.Split(got, "\n") {
		assert.LessOrEqual(t, len(line), 110, "no rendered line may run away: %q", line)
	}
}

// A configured timeout has to reach the scrape. Dropping it falls back to the
// package default, which is an order of magnitude longer.
func TestRunHonoursTheConfiguredScrapeTimeout(t *testing.T) {
	stalled, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = stalled.Close() })

	// Accept but never reply, so only the deadline can end the scrape.
	go func() {
		conn, err := stalled.Accept()
		if err != nil {
			return
		}
		t.Cleanup(func() { _ = conn.Close() })
	}()

	cfg := testConfig()
	cfg.ScrapeURIs = []string{"tcp://" + stalled.Addr().String() + "/status"}
	cfg.ScrapeTimeout = 200 * time.Millisecond

	started := time.Now()
	err = Run(cfg, io.Discard)
	elapsed := time.Since(started)

	require.Error(t, err)
	assert.Less(t, elapsed, 2*time.Second,
		"the configured timeout must be used, not the much longer package default")
}

func TestRunWritesTheSpewDump(t *testing.T) {
	cfg := testConfig()
	cfg.Output = "spew"

	var out bytes.Buffer
	_ = Run(cfg, &out)

	assert.Contains(t, out.String(), "PoolManager", "spew dumps the Go structure")
	assert.Contains(t, out.String(), "tcp://127.0.0.1:1/status")
}

func TestRunRejectsAnUnknownOutputFormat(t *testing.T) {
	cfg := testConfig()
	cfg.Output = "yaml"

	var out bytes.Buffer
	err := Run(cfg, &out)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "yaml", "the error names the format that was asked for")
	assert.Empty(t, out.String(), "an unusable format must not emit a partial document")
}

// A scrape failure is reported, but whatever was collected is still written: a
// partial result is worth having, and the caller decides via the exit code.
func TestRunStillWritesOutputWhenAScrapeFails(t *testing.T) {
	cfg := testConfig()

	var out bytes.Buffer
	err := Run(cfg, &out)

	require.Error(t, err)
	assert.NotEmpty(t, out.String(), "output must survive a failed scrape")
}

func TestRunAddsEveryScrapeURI(t *testing.T) {
	cfg := testConfig()
	cfg.ScrapeURIs = []string{"tcp://127.0.0.1:1/status", "tcp://127.0.0.2:1/status"}

	var out bytes.Buffer
	_ = Run(cfg, &out)

	var decoded phpfpm.PoolManager
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	require.Len(t, decoded.Pools, 2, "one pool per configured URI")
}

// A malformed URI used to panic inside the scrape goroutine.
func TestRunReportsAMalformedScrapeURI(t *testing.T) {
	cfg := testConfig()
	cfg.ScrapeURIs = []string{"tcp://127.0.0.1:90 00/status"}

	var out bytes.Buffer

	require.NotPanics(t, func() {
		err := Run(cfg, &out)
		assert.Error(t, err)
	})
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, assert.AnError }

func TestRunReportsAFailedWrite(t *testing.T) {
	cfg := testConfig()

	err := Run(cfg, failingWriter{})

	require.Error(t, err)
	assert.ErrorIs(t, err, assert.AnError, "a write failure must not be mistaken for a scrape failure")
}
