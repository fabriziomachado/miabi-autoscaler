// miabi-autoscaler scales Miabi "service" apps horizontally from their HTTP traffic.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var version = "dev"

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() { os.Exit(run()) }

func run() int {
	cfgPath := flag.String("config", env("CONFIG_FILE", "/etc/miabi-autoscaler/config.yaml"), "arquivo de configuracao YAML")
	listen := flag.String("listen", env("LISTEN_ADDR", ":8080"), "endereco de /healthz, /metrics e /status")
	check := flag.Bool("check", false, "valida a configuracao e sai")
	showVersion := flag.Bool("version", false, "mostra a versao e sai")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return 0
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, raw, err := LoadConfig(*cfgPath)
	if err != nil {
		log.Error("configuracao invalida", "path", *cfgPath, "err", err)
		return 2
	}
	url := env("MIABI_URL", cfg.Miabi.URL)
	ws := env("MIABI_WORKSPACE", cfg.Miabi.Workspace)
	token := os.Getenv("MIABI_TOKEN")
	if url == "" || ws == "" || token == "" {
		log.Error("faltam dados de acesso: defina miabi.url/miabi.workspace (ou MIABI_URL/MIABI_WORKSPACE) e MIABI_TOKEN")
		return 2
	}
	if *check {
		log.Info("configuracao OK", "apps", len(cfg.Apps), "url", url, "workspace", ws, "dry_run", cfg.DryRun)
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	metrics := NewMetrics(version)
	ctrl := NewController(NewClient(url, ws, token), metrics, log, *cfgPath, cfg, raw)
	go ctrl.Run(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !ctrl.Healthy() {
			http.Error(w, "loop parado", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		metrics.Write(w)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(ctrl.Snapshot())
	})
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	log.Info("miabi-autoscaler iniciado", "version", version, "apps", len(cfg.Apps), "listen", *listen, "dry_run", cfg.DryRun)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("servidor HTTP falhou", "err", err)
		return 1
	}
	log.Info("encerrado")
	return 0
}
