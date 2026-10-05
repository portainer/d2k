package streams

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/api/pkg/stdcopy"
)

// The frames must decode with moby's own reader, which Docker clients use.
func TestMultiplexedDecodesWithStdCopy(t *testing.T) {
	var buf bytes.Buffer
	out, errw := Multiplexed(&buf, Stdout), Multiplexed(&buf, Stderr)
	_, _ = out.Write([]byte("hello "))
	_, _ = errw.Write([]byte("oops"))
	_, _ = out.Write([]byte("world"))
	_, _ = out.Write(nil)

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, &buf); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "hello world" || stderr.String() != "oops" {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestMultiplexedConcurrentWritesStayFramed(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	w := lockedWriter{&mu, &buf}
	var wg sync.WaitGroup
	for _, stream := range []byte{Stdout, Stderr} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mw := Multiplexed(w, stream)
			for range 200 {
				_, _ = mw.Write([]byte("0123456789"))
			}
		}()
	}
	wg.Wait()
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, &buf); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 2000 || stderr.Len() != 2000 {
		t.Fatalf("stdout %d bytes, stderr %d bytes, want 2000 each", stdout.Len(), stderr.Len())
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestHijackResponseHead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, in, err := Hijack(w, r, r.URL.Query().Get("tty") == "1")
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		line, _ := in.ReadString('\n') // echo what the client sends after the head
		_, _ = conn.Write([]byte(line))
	}))
	defer srv.Close()

	for _, tt := range []struct {
		name, query, upgrade string
		wantStatus, wantType string
	}{
		{"upgrade, TTY", "?tty=1", "tcp", "HTTP/1.1 101 UPGRADED", "application/vnd.docker.raw-stream"},
		{"upgrade, no TTY", "", "tcp", "HTTP/1.1 101 UPGRADED", "application/vnd.docker.multiplexed-stream"},
		{"no upgrade", "", "", "HTTP/1.1 200 OK", "application/vnd.docker.multiplexed-stream"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			req := "POST /exec/x/start" + tt.query + " HTTP/1.1\r\nHost: d2k\r\nContent-Length: 0\r\n"
			if tt.upgrade != "" {
				req += "Connection: Upgrade\r\nUpgrade: " + tt.upgrade + "\r\n"
			}
			_, _ = conn.Write([]byte(req + "\r\nping\n"))

			r := bufio.NewReader(conn)
			resp, err := http.ReadResponse(r, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.Proto + " " + resp.Status; got != tt.wantStatus {
				t.Errorf("status = %q, want %q", got, tt.wantStatus)
			}
			if got := resp.Header.Get("Content-Type"); got != tt.wantType {
				t.Errorf("Content-Type = %q, want %q", got, tt.wantType)
			}
			echoed, _ := r.ReadString('\n')
			if echoed != "ping\n" {
				t.Errorf("bytes sent after the request head = %q, want them readable after hijack", echoed)
			}
		})
	}
}
