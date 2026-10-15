// Package streams implements the Docker Engine API's stream formats: the
// multiplexed framing of stdout and stderr, and taking over the connection
// for attach and exec.
package streams

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// Stream identifiers in a multiplexed stream.
const (
	Stdout byte = 1
	Stderr byte = 2
)

// Multiplexed returns a writer that frames each write for the given stream
// as Docker does: a header of the stream, three zero bytes and the payload
// length (big-endian uint32), then the payload. Each frame is one Write on w,
// so writers for stdout and stderr can share a w that is safe for concurrent
// writes, such as a net.Conn.
func Multiplexed(w io.Writer, stream byte) io.Writer {
	return muxWriter{w: w, stream: stream}
}

type muxWriter struct {
	w      io.Writer
	stream byte
}

func (m muxWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	frame := make([]byte, 8+len(p))
	frame[0] = m.stream
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(p)))
	copy(frame[8:], p)
	if _, err := m.w.Write(frame); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Hijack takes over the connection of an attach or exec request and writes
// the response head as the daemon does: 101 UPGRADED when the client asked
// to upgrade to "tcp", 200 otherwise; a raw stream for a TTY, a multiplexed
// one otherwise. The returned reader holds what the client sends next,
// including bytes it sent with the request.
func Hijack(w http.ResponseWriter, r *http.Request, tty bool) (net.Conn, *bufio.Reader, error) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("the connection cannot be taken over")
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, fmt.Errorf("unable to take over the connection: %w", err)
	}

	contentType := "application/vnd.docker.multiplexed-stream"
	if tty {
		contentType = "application/vnd.docker.raw-stream"
	}
	head := "HTTP/1.1 200 OK\r\nContent-Type: " + contentType + "\r\n\r\n"
	if strings.EqualFold(r.Header.Get("Upgrade"), "tcp") {
		head = "HTTP/1.1 101 UPGRADED\r\nContent-Type: " + contentType + "\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"
	}
	if _, err := conn.Write([]byte(head)); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, rw.Reader, nil
}
