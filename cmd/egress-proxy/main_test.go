package main

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxyServerHealthcheck(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewProxyServer("8888", []string{"allowed.com"}, logger)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "healthy") {
		t.Fatalf("expected healthy response, got %s", w.Body.String())
	}
}

func TestProxyServerDisallowedDomain(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewProxyServer("8888", []string{"allowed.com"}, logger)

	// Blocked CONNECT
	req := httptest.NewRequest(http.MethodConnect, "http://evil.com:443", nil)
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden, got %d", w.Code)
	}

	// Blocked GET
	req2 := httptest.NewRequest(http.MethodGet, "http://evil.com/data", nil)
	w2 := httptest.NewRecorder()
	proxy.ServeHTTP(w2, req2)

	if w2.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden, got %d", w2.Code)
	}
}

func TestProxyServerConnectAllowedDomain(t *testing.T) {
	// Create mock upstream echo server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	mockUpstreamHost, _, _ := net.SplitHostPort(listener.Addr().String())

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		_, _ = conn.Write(append([]byte("ECHO: "), buf[:n]...))
	}()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewProxyServer("8888", []string{mockUpstreamHost, "127.0.0.1"}, logger)

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Connect to proxy and issue CONNECT request
	conn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	req := "CONNECT " + listener.Addr().String() + " HTTP/1.1\r\nHost: " + listener.Addr().String() + "\r\n\r\n"
	_, err = conn.Write([]byte(req))
	if err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(statusLine, "200 Connection Established") {
		t.Fatalf("expected 200 Connection Established, got %s", statusLine)
	}

	// Read past trailing empty line
	_, _ = reader.ReadString('\n')

	// Test bidirectional tunnel
	testData := "hello-via-proxy"
	_, _ = conn.Write([]byte(testData))

	respBuf := make([]byte, 1024)
	n, err := conn.Read(respBuf)
	if err != nil {
		t.Fatal(err)
	}

	expected := "ECHO: " + testData
	if string(respBuf[:n]) != expected {
		t.Fatalf("expected '%s', got '%s'", expected, string(respBuf[:n]))
	}
}
