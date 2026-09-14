package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var defaultAllowlist = []string{
	"thedemontuan.cloudflareaccess.com",
	"img.vietqr.io",
	"api.vietqr.io",
}

type ProxyServer struct {
	allowedDomains map[string]bool
	port           string
	logger         *slog.Logger
}

func NewProxyServer(port string, allowedDomains []string, logger *slog.Logger) *ProxyServer {
	domainsMap := make(map[string]bool)
	for _, d := range allowedDomains {
		cleaned := strings.ToLower(strings.TrimSpace(d))
		if cleaned != "" {
			domainsMap[cleaned] = true
		}
	}
	return &ProxyServer{
		allowedDomains: domainsMap,
		port:           port,
		logger:         logger,
	}
}

func (p *ProxyServer) isAllowed(targetHost string) bool {
	host := strings.ToLower(strings.TrimSpace(targetHost))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return p.allowedDomains[host]
}

func (p *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" || r.URL.Path == "/health" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy","service":"egress-proxy"}`))
		return
	}

	target := r.Host
	if target == "" {
		target = r.URL.Host
	}

	if !p.isAllowed(target) {
		p.logger.Warn("Egress proxy blocked destination",
			"method", r.Method,
			"target", target,
			"client_ip", r.RemoteAddr,
		)
		http.Error(w, fmt.Sprintf("Egress proxy forbidden: destination '%s' not in strict allowlist", target), http.StatusForbidden)
		return
	}

	if r.Method == http.MethodConnect {
		p.handleConnect(w, r, target)
		return
	}

	p.handleHTTP(w, r)
}

func (p *ProxyServer) handleConnect(w http.ResponseWriter, r *http.Request, target string) {
	if !strings.Contains(target, ":") {
		target = target + ":443"
	}

	destConn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		p.logger.Error("Egress proxy failed to dial destination", "target", target, "error", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		destConn.Close()
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		destConn.Close()
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	go transfer(destConn, clientConn)
	go transfer(clientConn, destConn)
}

func (p *ProxyServer) handleHTTP(w http.ResponseWriter, r *http.Request) {
	r.RequestURI = ""
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func transfer(destination io.WriteCloser, source io.ReadCloser) {
	defer destination.Close()
	defer source.Close()
	_, _ = io.Copy(destination, source)
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "Run local healthcheck against egress-proxy")
	flag.Parse()

	port := os.Getenv("PROXY_PORT")
	if port == "" {
		port = "8888"
	}

	if *healthcheck {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/healthz", port))
		if err == nil && resp.StatusCode == http.StatusOK {
			os.Exit(0)
		}
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	allowed := defaultAllowlist
	if custom := os.Getenv("ALLOWED_DOMAINS"); custom != "" {
		allowed = strings.Split(custom, ",")
	}

	server := NewProxyServer(port, allowed, logger)
	httpServer := &http.Server{
		Addr:         "0.0.0.0:" + port,
		Handler:      server,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	logger.Info("Starting Gateway Egress Filtering Proxy",
		"port", port,
		"allowed_domains", allowed,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Egress proxy server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("Shutting down egress proxy gracefully...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}
