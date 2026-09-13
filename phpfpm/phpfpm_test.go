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

package phpfpm

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain installs the logger exactly once. SetLogger writes a package-global
// that background watcher goroutines read, so calling it per test races with any
// goroutine an earlier test left running.
func TestMain(m *testing.M) {
	SetLogger(testLogger{})
	os.Exit(m.Run())
}

// logSink optionally receives formatted log messages. Swapping the sink rather
// than the logger keeps the package-global write in TestMain.
var logSink atomic.Pointer[chan string]

type testLogger struct{}

func (testLogger) publish(format string, ar ...any) {
	ch := logSink.Load()
	if ch == nil {
		return
	}
	select {
	case *ch <- fmt.Sprintf(format, ar...):
	default:
	}
}

func (l testLogger) Info(...any)                     {}
func (l testLogger) Infof(format string, ar ...any)  { l.publish(format, ar...) }
func (l testLogger) Debug(...any)                    {}
func (l testLogger) Debugf(format string, ar ...any) { l.publish(format, ar...) }
func (l testLogger) Error(...any)                    {}
func (l testLogger) Errorf(format string, ar ...any) { l.publish(format, ar...) }

// captureLogs routes log messages to a channel for the duration of the test. The
// log is the only synchronisation point a test has with the watcher goroutine
// that owns the PoolManager.
func captureLogs(t *testing.T) chan string {
	t.Helper()

	ch := make(chan string, 256)
	logSink.Store(&ch)
	t.Cleanup(func() { logSink.Store(nil) })

	return ch
}

// requireLogged waits for a message containing want, failing the test if the
// goroutine never gets there.
func requireLogged(t *testing.T, logs chan string, want string) {
	t.Helper()

	deadline := time.After(10 * time.Second)

	for {
		select {
		case msg := <-logs:
			if strings.Contains(msg, want) {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a log message containing %q", want)
		}
	}
}

func TestPoolManagerUpdateScrapesEveryPool(t *testing.T) {
	pm := PoolManager{ScrapeTimeout: 50 * time.Millisecond}
	pm.Add("tcp://127.0.0.1:1/status", "")
	pm.Add("tcp://127.0.0.1:1/status", "")

	require.NoError(t, pm.Update(), "Update reports per-pool failures on the pools, not as a return value")

	for i := range pm.Pools {
		assert.Error(t, pm.Pools[i].ScrapeError, "every pool must have been visited")
		assert.Equal(t, int64(1), pm.Pools[i].ScrapeFailures)
	}
}

func TestPoolManagerRemoveDropsMatchingPool(t *testing.T) {
	pm := PoolManager{}
	pm.Add("tcp://10.0.0.1:9000/status", "php-fpm-0")
	pm.Add("tcp://10.0.0.2:9000/status", "php-fpm-1")
	exporter := NewExporter(pm)

	pm.Remove(exporter, "tcp://10.0.0.1:9000/status")

	require.Len(t, pm.Pools, 1)
	assert.Equal(t, "tcp://10.0.0.2:9000/status", pm.Pools[0].Address)
	assert.Equal(t, pm.Pools, exporter.PoolManager.Pools, "the exporter must see the removal")
}

// Two pods can briefly share a URI (a pod IP is reused after a fast reschedule).
// Removal must drop every match, and must not read Pools while mutating it: the
// range bound is fixed at loop entry, so a concurrent shrink indexes past len.
func TestPoolManagerRemoveDropsEveryMatchingPool(t *testing.T) {
	pm := PoolManager{}
	pm.Add("tcp://10.0.0.1:9000/status", "php-fpm-0")
	pm.Add("tcp://10.0.0.1:9000/status", "php-fpm-1")
	pm.Add("tcp://10.0.0.2:9000/status", "php-fpm-2")
	exporter := NewExporter(pm)

	pm.Remove(exporter, "tcp://10.0.0.1:9000/status")

	require.Len(t, pm.Pools, 1)
	assert.Equal(t, "tcp://10.0.0.2:9000/status", pm.Pools[0].Address)
	assert.Equal(t, "php-fpm-2", pm.Pools[0].Pod)
}

// The debug log is how an operator sees which pools a pod deletion dropped.
func TestPoolManagerRemoveLogsEveryRemoval(t *testing.T) {
	logs := captureLogs(t)

	pm := PoolManager{}
	pm.Add("tcp://10.0.0.1:9000/status", "php-fpm-0")
	pm.Add("tcp://10.0.0.1:9000/status", "php-fpm-1")
	pm.Add("tcp://10.0.0.2:9000/status", "php-fpm-2")

	pm.Remove(NewExporter(pm), "tcp://10.0.0.1:9000/status")
	close(logs)

	var removed []string
	summaries := 0

	for msg := range logs {
		switch {
		case strings.HasPrefix(msg, "Removing pool: "):
			removed = append(removed, msg)
		case strings.HasPrefix(msg, "Removed pools in "):
			summaries++
		}
	}

	assert.Equal(t, []string{
		"Removing pool: tcp://10.0.0.1:9000/status",
		"Removing pool: tcp://10.0.0.1:9000/status",
	}, removed, "one line per removed pool, naming the URI")
	assert.Equal(t, 1, summaries, "one summary line per Remove call")
}

func TestPoolManagerRemoveIgnoresUnknownURI(t *testing.T) {
	pm := PoolManager{}
	pm.Add("tcp://10.0.0.1:9000/status", "php-fpm-0")
	exporter := NewExporter(pm)

	pm.Remove(exporter, "tcp://10.0.0.9:9000/status")

	require.Len(t, pm.Pools, 1)
	assert.Equal(t, "tcp://10.0.0.1:9000/status", pm.Pools[0].Address)
}

func TestCountProcessState(t *testing.T) {
	processes := []PoolProcess{
		{State: PoolProcessRequestIdle},
		{State: PoolProcessRequestRunning},
		{State: PoolProcessRequestReadingHeaders},
		{State: PoolProcessRequestInfo},
		{State: PoolProcessRequestFinishing},
		{State: PoolProcessRequestEnding},
	}

	active, idle, total := CountProcessState(processes)

	assert.Equal(t, int64(2), active, "active processes")
	assert.Equal(t, int64(1), idle, "idle processes")
	assert.Equal(t, int64(3), total, "total processes")
}

func TestResolveTimeout(t *testing.T) {
	assert.Equal(t, defaultScrapeTimeout, resolveTimeout(0), "zero falls back to default")
	assert.Equal(t, defaultScrapeTimeout, resolveTimeout(-time.Second), "negative falls back to default")
	assert.Equal(t, 5*time.Second, resolveTimeout(5*time.Second), "positive passes through")
}

// https://github.com/hipages/php-fpm_exporter/issues/10
func TestCannotUnmarshalNumberIssue10(t *testing.T) {
	pool := Pool{}
	content := []byte(`{
	   "pool":"www",
	   "process manager":"dynamic",
	   "start time":1519474655,
	   "start since":302035,
	   "accepted conn":44144,
	   "listen queue":0,
	   "max listen queue":1,
	   "listen queue len":128,
	   "idle processes":1,
	   "active processes":1,
	   "total processes":2,
	   "max active processes":2,
	   "max children reached":0,
	   "slow requests":0,
	   "processes":[
		  {
			 "pid":23,
			 "state":"Idle",
			 "start time":1519474655,
			 "start since":302035,
			 "requests":22071,
			 "request duration":295,
			 "request method":"GET",
			 "request uri":"/status?json&full",
			 "content length":0,
			 "user":"-",
			 "script":"-",
			 "last request cpu":0.00,
			 "last request memory":2097152
		  },
		  {
			 "pid":24,
			 "state":"Running",
			 "start time":1519474655,
			 "start since":302035,
			 "requests":22073,
			 "request duration":18446744073709550774,
			 "request method":"GET",
			 "request uri":"/status?json&full",
			 "content length":0,
			 "user":"-",
			 "script":"-",
			 "last request cpu":0.00,
			 "last request memory":0
		  }
	   ]
    }`)

	err := json.Unmarshal(content, &pool)

	assert.Nil(t, err, "successfully unmarshal on invalid 'request duration'")
	assert.Equal(t, int(pool.Processes[0].RequestDuration), 295, "request duration set to 0 because it couldn't be deserialized")
	assert.Equal(t, int(pool.Processes[1].RequestDuration), 0, "request duration set to 0 because it couldn't be deserialized")
}

// https://github.com/hipages/php-fpm_exporter/issues/24
func TestInvalidCharacterIssue24(t *testing.T) {
	// todo: Implement fcgi client dependency injection to allow testing of Pool.Update
}

func TestJsonResponseFixer(t *testing.T) {
	pool := Pool{}
	content := []byte(`{"pool":"www","process manager":"dynamic","start time":1528367006,"start since":15073840,"accepted conn":1577112,"listen queue":0,"max listen queue":0,"listen queue len":0,"idle processes":16,"active processes":1,"total processes":17,"max active processes":15,"max children reached":0,"slow requests":0, "processes":[{"pid":15873,"state":"Idle","start time":1543354120,"start since":86726,"requests":853,"request duration":5721,"request method":"GET","request uri":"/vbseo.php?ALTERNATE_TEMPLATES=|%20echo%20"Content-Type:%20text%2Fhtml"%3Becho%20""%20%3B%20id%00","content length":0,"user":"-","script":"/www/forum.example.com/vbseo.php","last request cpu":349.59,"last request memory":786432},{"pid":123,"state":"Idle","start time":1543354120,"start since":86726,"requests":853,"request duration":5721,"request method":"GET","request uri":"123/vbseo.php?ALTERNATE_TEMPLATES=|%20echo%20"Content-Type:%20text%2Fhtml"%3Becho%20""%20%3B%20id%00","content length":0,"user":"-","script":"/www/forum.example.com/vbseo.php","last request cpu":349.59,"last request memory":786432}]}`)

	content = JSONResponseFixer(content)

	err := json.Unmarshal(content, &pool)

	assert.Nil(t, err, "successfully unmarshal on invalid 'request uri'")
	assert.Equal(t, pool.Processes[0].RequestURI, `/vbseo.php?ALTERNATE_TEMPLATES=|%20echo%20"Content-Type:%20text%2Fhtml"%3Becho%20""%20%3B%20id%00`, "request uri couldn't be deserialized")
	assert.Equal(t, pool.Processes[1].RequestURI, `123/vbseo.php?ALTERNATE_TEMPLATES=|%20echo%20"Content-Type:%20text%2Fhtml"%3Becho%20""%20%3B%20id%00`, "request uri couldn't be deserialized")
}

func TestParseURL(t *testing.T) {
	var uris = []struct {
		in  string
		out []string
		err error
	}{
		{"tcp://127.0.0.1:9000/status", []string{"tcp", "127.0.0.1:9000", "/status"}, nil},
		{"tcp://127.0.0.1", []string{"tcp", "127.0.0.1", ""}, nil},
		{"unix:///tmp/php.sock;/status", []string{"unix", "/tmp/php.sock", "/status"}, nil},
		{"unix:///tmp/php.sock", []string{"unix", "/tmp/php.sock", ""}, nil},
	}

	for _, u := range uris {
		scheme, address, path, err := parseURL(u.in)
		assert.Equal(t, u.err, err)
		assert.Equal(t, u.out, []string{scheme, address, path})
	}
}
