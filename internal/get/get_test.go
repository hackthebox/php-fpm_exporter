// Copyright 2026 Hack The Box
// SPDX-License-Identifier: Apache-2.0

package get

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
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
