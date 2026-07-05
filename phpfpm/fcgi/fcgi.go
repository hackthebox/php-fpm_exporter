// Copyright 2026 Hack The Box
// SPDX-License-Identifier: Apache-2.0

// Package fcgi implements the minimal slice of the FastCGI client protocol
// needed to scrape a PHP-FPM status page. Every request runs under a hard
// connection deadline, so an unresponsive status endpoint returns an error
// instead of blocking the caller forever (which previously leaked a goroutine
// and a socket per scrape).
package fcgi

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sort"
	"time"
)

const (
	fcgiVersion1     = 1
	typeBeginRequest = 1
	typeEndRequest   = 3
	typeParams       = 4
	typeStdin        = 5
	typeStdout       = 6
	typeStderr       = 7
	roleResponder    = 1
	requestID        = 1
)

// Get performs a single FastCGI RESPONDER request to network/address, sending
// params as the CGI environment with an empty request body, and returns the
// response body with CGI headers stripped. The whole exchange is bounded by
// timeout via a connection deadline.
func Get(network, address string, params map[string]string, timeout time.Duration) ([]byte, error) {
	conn, err := net.DialTimeout(network, address, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	return exchange(conn, params)
}

func exchange(rw io.ReadWriter, params map[string]string) ([]byte, error) {
	if err := writeRequest(rw, params); err != nil {
		return nil, err
	}
	stdout, _, err := readResponse(rw)
	if err != nil {
		return nil, err
	}
	return stripCGIBody(stdout), nil
}

type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) record(typ uint8, content []byte) {
	if e.err != nil {
		return
	}
	e.err = writeRecord(e.w, typ, content)
}

func writeRequest(w io.Writer, params map[string]string) error {
	begin := []byte{0, roleResponder, 0, 0, 0, 0, 0, 0}
	ew := &errWriter{w: w}
	ew.record(typeBeginRequest, begin)
	ew.record(typeParams, encodeParams(params))
	ew.record(typeParams, nil)
	ew.record(typeStdin, nil)
	return ew.err
}

func writeRecord(w io.Writer, typ uint8, content []byte) error {
	length := len(content)
	header := [8]byte{
		fcgiVersion1, typ,
		byte((requestID >> 8) & 0xff), byte(requestID & 0xff),
		byte((length >> 8) & 0xff), byte(length & 0xff),
		0, 0,
	}
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(content)
	return err
}

func readResponse(r io.Reader) (stdout, stderr []byte, err error) {
	for {
		typ, content, err := readRecord(r)
		if errors.Is(err, io.EOF) {
			return stdout, stderr, nil
		}
		if err != nil {
			return stdout, stderr, err
		}
		if typ == typeEndRequest {
			return stdout, stderr, nil
		}
		if typ == typeStderr {
			stderr = append(stderr, content...)
			continue
		}
		stdout = append(stdout, content...)
	}
}

func readRecord(r io.Reader) (typ uint8, content []byte, err error) {
	var header [8]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint16(header[4:6])
	padding := header[6]

	buf := make([]byte, int(length)+int(padding))
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return header[1], buf[:length], nil
}

func encodeParams(params map[string]string) []byte {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf []byte
	for _, k := range keys {
		v := params[k]
		buf = append(buf, encodeLength(len(k))...)
		buf = append(buf, encodeLength(len(v))...)
		buf = append(buf, k...)
		buf = append(buf, v...)
	}
	return buf
}

func encodeLength(n int) []byte {
	if n < 128 {
		return []byte{byte(n & 0xff)}
	}
	return []byte{byte((n>>24)&0xff) | 0x80, byte((n >> 16) & 0xff), byte((n >> 8) & 0xff), byte(n & 0xff)}
}

func stripCGIBody(stdout []byte) []byte {
	sep := []byte("\r\n\r\n")
	for i := 0; i+len(sep) <= len(stdout); i++ {
		if string(stdout[i:i+len(sep)]) == string(sep) {
			return stdout[i+len(sep):]
		}
	}
	return stdout
}
