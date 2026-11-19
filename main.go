// Command delivery-board shows every change of an application as a run, from its pull request (or push) through
// the release to the last environment, and gates promotions between environments. It derives everything from
// GitHub and the cluster at every refresh: no database, no state of its own, so it can be restarted or replaced at
// any time and never disagrees with git.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"
)

func main() {
	cfgPath := flag.String("config", "/etc/delivery-board/config.yaml", "pipelines")
	addr := flag.String("addr", ":8080", "listen address")
	check := flag.Bool("check", false, "validate the config and exit")
	printCfg := flag.Bool("print-config", false, "print the config with services / defaults expanded, and exit")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if *printCfg {
		src, err := os.ReadFile(*cfgPath)
		if err == nil {
			src, err = expandConfig(src)
		}
		if err != nil {
			slog.Error("config", "err", err)
			os.Exit(2)
		}
		os.Stdout.Write(src)
		return
	}
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(2)
	}
	if *check {
		slog.Info("config ok", "pipelines", len(cfg.Pipelines))
		return
	}
	b := NewBoard(cfg)
	go b.Loop()
	srv := &http.Server{Addr: *addr, Handler: b.Handler(), ReadHeaderTimeout: 10 * time.Second}
	slog.Info("listening", "addr", *addr, "pipelines", len(cfg.Pipelines), "promote", b.CanPromote())
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}
