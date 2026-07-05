// Copyright 2026 Hack The Box
// SPDX-License-Identifier: Apache-2.0

package fcgi

import (
	"bytes"
	"errors"
	"net"
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
