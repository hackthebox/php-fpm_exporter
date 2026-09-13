// Copyright 2026 Hack The Box
// SPDX-License-Identifier: Apache-2.0

package fcgi

import (
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeLength(t *testing.T) {
	assert.Equal(t, []byte{5}, encodeLength(5), "short length is a single byte")
	assert.Equal(t, []byte{0x80, 0x00, 0x00, 0xC8}, encodeLength(200), "length >=128 uses 4 bytes with high bit set")
}

func TestEncodeParams(t *testing.T) {
	// Keys are emitted sorted for deterministic output.
	got := encodeParams(map[string]string{"C": "DD", "A": "B"})
	want := []byte{1, 1, 'A', 'B', 1, 2, 'C', 'D', 'D'}
	assert.Equal(t, want, got)
}

// Map iteration order is randomised, so two keys leave an unsorted encoder
// passing about half the time. Enough keys to make that vanishingly unlikely,
// repeated so a single lucky ordering cannot carry the test.
func TestEncodeParamsOrdersKeysDeterministically(t *testing.T) {
	params := map[string]string{}
	for _, k := range []string{"H", "B", "F", "A", "G", "C", "E", "D"} {
		params[k] = "v"
	}

	var want []byte
	for _, k := range []string{"A", "B", "C", "D", "E", "F", "G", "H"} {
		want = append(want, 1, 1, k[0], 'v')
	}

	for range 32 {
		assert.Equal(t, want, encodeParams(params))
	}
}

func TestEncodeParamsIsEmptyForNoParams(t *testing.T) {
	assert.Empty(t, encodeParams(map[string]string{}))
	assert.Empty(t, encodeParams(nil))
}

// A PHP-FPM status query carries a QUERY_STRING that can exceed the one-byte
// length limit, so both the key and the value must go through encodeLength.
func TestEncodeParamsUsesFourByteLengthsForLongValues(t *testing.T) {
	value := strings.Repeat("x", 200)

	got := encodeParams(map[string]string{"K": value})

	want := append([]byte{1, 0x80, 0x00, 0x00, 0xC8, 'K'}, value...)
	assert.Equal(t, want, got)
}

func TestWriteReadRecordRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeRecord(&buf, typeStdout, []byte("hello")))

	typ, content, err := readRecord(&buf)
	require.NoError(t, err)
	assert.Equal(t, uint8(typeStdout), typ)
	assert.Equal(t, []byte("hello"), content)
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }

func TestWriteRecordWriteError(t *testing.T) {
	assert.Error(t, writeRecord(failWriter{}, typeStdout, []byte("x")))
}

func TestWriteRequestReturnsWriteError(t *testing.T) {
	// First record fails, so the remaining records must short-circuit.
	err := writeRequest(failWriter{}, map[string]string{"A": "B"})
	assert.Error(t, err)
}

func TestReadRecordTruncatedHeader(t *testing.T) {
	_, _, err := readRecord(bytes.NewReader([]byte{1, 6, 0}))
	assert.Error(t, err, "a header shorter than 8 bytes is an error")
}

func TestReadRecordTruncatedContent(t *testing.T) {
	// Header claims 10 content bytes but only 2 follow.
	hdr := []byte{1, typeStdout, 0, 1, 0, 10, 0, 0, 'a', 'b'}
	_, _, err := readRecord(bytes.NewReader(hdr))
	assert.Error(t, err)
}

func TestReadRecordSkipsPadding(t *testing.T) {
	// content "hi" (len 2) + 3 padding bytes; a following record must still be readable.
	var buf bytes.Buffer
	buf.Write([]byte{1, typeStdout, 0, 1, 0, 2, 3, 0, 'h', 'i', 0, 0, 0})
	require.NoError(t, writeRecord(&buf, typeEndRequest, nil))

	typ, content, err := readRecord(&buf)
	require.NoError(t, err)
	assert.Equal(t, uint8(typeStdout), typ)
	assert.Equal(t, []byte("hi"), content)

	typ2, _, err := readRecord(&buf)
	require.NoError(t, err, "padding must be consumed so the next record parses")
	assert.Equal(t, uint8(typeEndRequest), typ2)
}

func TestReadResponseConcatenatesStdoutUntilEndRequest(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeRecord(&buf, typeStdout, []byte("foo")))
	require.NoError(t, writeRecord(&buf, typeStderr, []byte("warn")))
	require.NoError(t, writeRecord(&buf, typeStdout, []byte("bar")))
	require.NoError(t, writeRecord(&buf, typeEndRequest, []byte{0, 0, 0, 0, 0, 0, 0, 0}))

	stdout, stderr, err := readResponse(&buf)
	require.NoError(t, err)
	assert.Equal(t, []byte("foobar"), stdout)
	assert.Equal(t, []byte("warn"), stderr)
}

func TestReadResponseStopsOnEOF(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeRecord(&buf, typeStdout, []byte("foo")))
	stdout, _, err := readResponse(&buf)
	require.NoError(t, err, "a clean EOF after data is not an error")
	assert.Equal(t, []byte("foo"), stdout)
}

func TestStripCGIBody(t *testing.T) {
	in := []byte("Content-type: application/json\r\n\r\n{\"pool\":\"www\"}")
	assert.Equal(t, []byte(`{"pool":"www"}`), stripCGIBody(in))
	assert.Equal(t, []byte("nobody"), stripCGIBody([]byte("nobody")), "no header separator returns input unchanged")
}

func TestStripCGIBodyBoundaries(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "only the first separator splits",
			// A JSON body can contain \r\n\r\n; splitting on a later one would
			// truncate it.
			in:   "Content-type: text/plain\r\n\r\nfirst\r\n\r\nsecond",
			want: "first\r\n\r\nsecond",
		},
		{name: "no headers", in: "\r\n\r\n{}", want: "{}"},
		{name: "empty body", in: "Status: 200\r\n\r\n", want: ""},
		{name: "empty input", in: "", want: ""},
		{
			name: "truncated separator is not a separator",
			in:   "Status: 200\r\n\r",
			want: "Status: 200\r\n\r",
		},
		{
			name: "bare newlines are not a separator",
			in:   "Status: 200\n\n{}",
			want: "Status: 200\n\n{}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, string(stripCGIBody([]byte(tt.in))))
		})
	}
}

// fakeFPM listens and replies with a canned FastCGI CGI response, then closes.
func fakeFPM(t *testing.T, body string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Drain the request before replying: closing with it unread RSTs the
		// client's in-flight write (flaky "connection reset by peer").
		for {
			typ, _, err := readRecord(conn)
			if err != nil || typ == typeStdin {
				break
			}
		}
		_ = writeRecord(conn, typeStdout, []byte("Content-type: application/json\r\n\r\n"+body))
		_ = writeRecord(conn, typeEndRequest, []byte{0, 0, 0, 0, 0, 0, 0, 0})
	}()
	return ln.Addr().String()
}

func TestGetReturnsBody(t *testing.T) {
	addr := fakeFPM(t, `{"pool":"www"}`)

	out, err := Get("tcp", addr, map[string]string{"SCRIPT_NAME": "/status"}, 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, []byte(`{"pool":"www"}`), out)
}

// The regression test for the leak: a status endpoint that accepts but never
// replies must NOT block forever. The deadline must abort the read.
func TestGetTimesOutOnUnresponsiveStatus(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Hold the connection open, never write a response.
		t.Cleanup(func() { _ = conn.Close() })
	}()

	start := time.Now()
	_, err = Get("tcp", ln.Addr().String(), map[string]string{"SCRIPT_NAME": "/status"}, 200*time.Millisecond)
	elapsed := time.Since(start)

	require.Error(t, err, "an unresponsive status socket must return an error, not hang")
	assert.Less(t, elapsed, 2*time.Second, "must abort near the deadline, not block")
}

func TestGetDialError(t *testing.T) {
	// Nothing listening on this address.
	_, err := Get("tcp", "127.0.0.1:1", map[string]string{}, 200*time.Millisecond)
	assert.Error(t, err)
}
