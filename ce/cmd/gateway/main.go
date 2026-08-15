// Command gateway runs the unified API gateway (ADR-19): the single frontend
// entry that routes /v1/* to Go services and /v2/agents/* to the Python
// agent plane. Backends are configured via environment (design/60).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"adc.dev/ce/internal/gateway"
)

func main() {
	addr := getenv("ADC_HTTP_ADDR", ":8080")
	cfg := gateway.Config{
		Addr: addr,
		Backends: gateway.Backends{
			Admin:     getenv("ADC_BACKEND_ADMIN", "http://admin:8080"),
			AgentAPI:  getenv("ADC_BACKEND_AGENTAPI", "http://adc:8080"),
			Connector: getenv("ADC_BACKEND_CONNECTOR", "http://adc:8080"),
			Approval:  getenv("ADC_BACKEND_APPROVAL", "http://adc:8080"),
			PyAgent:   getenv("ADC_BACKEND_PYAGENT", "http://py-agent:18081"),
		},
		Version:             "0.1.0",
		UpstreamTimeout:     45 * time.Second,
		HealthProbeTimeout:  2 * time.Second,
		ReadHeaderTimeout:   10 * time.Second,
		IdleTimeout:         120 * time.Second,
		ErrorLog:            slog.NewLogLogger(slog.NewTextHandler(os.Stderr, nil), slog.LevelError),
	}

	srv, err := gateway.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}

	// Root index for browser visits; all API traffic falls through to the
	// gateway routes untouched.
	outer := http.NewServeMux()
	outer.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>ADC Gateway</title></head><body>
<h1>ADC 统一 API 网关</h1>
<p>前端唯一业务入口。后端 Go/Python 服务经此路由，对客户端无感。</p>
<ul>
<li><a href="/healthz">/healthz</a> 网关与后端聚合健康检查</li>
<li>GET /v1/agent/mcp/tools（Agent 工具聚合，需 X-ADC-Key）</li>
<li>POST /v1/agent/mcp/tools/call（工具调用 + HITL，需 X-ADC-Key）</li>
<li>GET /v1/devices/tunnel（设备 WSS 隧道）</li>
<li>/v2/agents/evals/*（Python Agent 面评测，经网关路由）</li>
</ul></body></html>`)
	})
	outer.Handle("/", srv.Handler)
	srv.Handler = outer

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("adc gateway listening", "addr", addr, "pyagent", cfg.Backends.PyAgent)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "fatal:", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down gateway")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
