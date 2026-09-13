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

	require.Error(t, pm.Update(), "every pool failed, so Update must say so")

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

// This is the path `php-fpm_exporter get --out json` takes. Pools is a slice, so
// its elements are addressable and encoding/json does reach the pointer-receiver
// MarshalJSON methods on the process fields.
func TestPoolManagerMarshalsToValidJSON(t *testing.T) {
	pm := PoolManager{Pools: []Pool{{
		Name:      "www",
		StartTime: timestamp(time.Unix(1519474655, 0)),
		Processes: []PoolProcess{{
			PID:             23,
			State:           PoolProcessRequestIdle,
			RequestDuration: 295,
		}},
	}}}

	content, err := json.Marshal(pm)
	require.NoError(t, err)

	var decoded PoolManager
	require.NoError(t, json.Unmarshal(content, &decoded),
		"the output must be valid JSON, not a formatted pointer: %s", content)

	require.Len(t, decoded.Pools, 1)
	require.Len(t, decoded.Pools[0].Processes, 1)
	assert.Equal(t, requestDuration(295), decoded.Pools[0].Processes[0].RequestDuration,
		"request duration must survive the round trip")
	assert.Equal(t, int64(1519474655), time.Time(decoded.Pools[0].StartTime).Unix())
}

func TestRequestDurationMarshalJSONEmitsTheNumber(t *testing.T) {
	tests := []struct {
		name string
		in   requestDuration
		want string
	}{
		{name: "typical", in: 295, want: "295"},
		{name: "zero", in: 0, want: "0"},
		{name: "large", in: 18446744073709, want: "18446744073709"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.in.MarshalJSON()

			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

// PHP-FPM counts a child as idle only while it is accepting; every other stage
// is active (sapi/fpm/fpm/fpm_status.c). Counting Finishing, Ending and Info as
// neither dropped them from the total, which is what made phpfpm_total_processes
// sag towards the active count under load.
// https://github.com/hipages/php-fpm_exporter/issues/322
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

	assert.Equal(t, int64(5), active, "every non-idle stage is active")
	assert.Equal(t, int64(1), idle, "idle processes")
	assert.Equal(t, int64(6), total, "no reported process may go uncounted")
}

func TestCountProcessStateCountsEveryReportedProcess(t *testing.T) {
	// Every stage fpm_request.c can report, plus the PHP 7.4 spelling.
	states := []string{
		PoolProcessRequestCreating,
		PoolProcessRequestIdle,
		PoolProcessRequestReadingHeaders,
		PoolProcessRequestInfo,
		PoolProcessRequestInfo74,
		PoolProcessRequestRunning,
		PoolProcessRequestEnding,
		PoolProcessRequestFinishing,
	}

	processes := make([]PoolProcess, 0, len(states))
	for _, state := range states {
		processes = append(processes, PoolProcess{State: state})
	}

	active, idle, total := CountProcessState(processes)

	assert.Equal(t, int64(len(states)), total, "total must equal the processes FPM reported")
	assert.Equal(t, int64(1), idle, "only Idle is idle")
	assert.Equal(t, int64(len(states)-1), active)
	assert.Equal(t, total, active+idle)
}

// https://github.com/hipages/php-fpm_exporter/issues/419
func TestCountProcessStateKnowsTheCreatingStage(t *testing.T) {
	logs := captureLogs(t)

	active, idle, total := CountProcessState([]PoolProcess{{State: PoolProcessRequestCreating}})

	assert.Equal(t, int64(1), active, "a child being created is not idle, so FPM counts it active")
	assert.Zero(t, idle)
	assert.Equal(t, int64(1), total)

	close(logs)
	for msg := range logs {
		assert.NotContains(t, msg, "Unknown process state", "Creating is a documented FPM stage")
	}
}

func TestCountProcessStateCountsUnknownStages(t *testing.T) {
	logs := captureLogs(t)

	active, idle, total := CountProcessState([]PoolProcess{{State: "Some Future Stage"}})

	assert.Equal(t, int64(1), active, "an unrecognised stage still exists, so it must not vanish from the total")
	assert.Zero(t, idle)
	assert.Equal(t, int64(1), total)

	close(logs)

	logged := false
	for msg := range logs {
		if strings.Contains(msg, "Unknown process state 'Some Future Stage'") {
			logged = true
		}
	}
	assert.True(t, logged, "an unrecognised stage must still be reported")
}

// url.Parse returns a nil *URL alongside its error, so touching the result in
// the error branch panics. It runs inside the scrape goroutine, so it took the
// whole process down.
func TestParseURLReturnsAnErrorRatherThanPanicking(t *testing.T) {
	scheme, address, path, err := parseURL("tcp://127.0.0.1:90 00/status")

	require.Error(t, err)
	assert.Empty(t, scheme)
	assert.Empty(t, address)
	assert.Empty(t, path)
}

func TestPoolUpdateReportsAMalformedURI(t *testing.T) {
	pool := Pool{Address: "tcp://127.0.0.1:90 00/status"}

	err := pool.Update(time.Second)

	require.Error(t, err)
	assert.Equal(t, err, pool.ScrapeError)
	assert.Equal(t, int64(1), pool.ScrapeFailures)
}

func TestPoolManagerUpdateReportsScrapeFailures(t *testing.T) {
	pm := PoolManager{ScrapeTimeout: 50 * time.Millisecond}
	pm.Add("tcp://127.0.0.1:1/status", "")

	err := pm.Update()

	require.Error(t, err, "a caller cannot otherwise tell that every target failed")
	assert.ErrorIs(t, err, pm.Pools[0].ScrapeError)
}

func TestPoolManagerUpdateReturnsNilWhenNothingToDo(t *testing.T) {
	pm := PoolManager{}

	assert.NoError(t, pm.Update())
}

// discardLogger is what the package global starts as, so every method has to be
// callable before a caller supplies a real logger.
func TestDiscardLoggerAcceptsEveryLevel(t *testing.T) {
	var l logger = discardLogger{}

	assert.NotPanics(t, func() {
		l.Info("info")
		l.Infof("%s", "infof")
		l.Debug("debug")
		l.Debugf("%s", "debugf")
		l.Error("error")
		l.Errorf("%s", "errorf")
	})
}

// The package logger is a global that library callers may never set. It used to
// start out nil, so the first unrecognised process state panicked.
func TestCountProcessStateWithoutALoggerSet(t *testing.T) {
	SetLogger(nil)

	assert.NotPanics(t, func() {
		CountProcessState([]PoolProcess{{State: "Some Future Stage"}})
	}, "a nil logger must be ignored rather than installed")
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
