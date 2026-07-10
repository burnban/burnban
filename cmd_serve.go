package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/syft8/burnban/internal/budget"
	"github.com/syft8/burnban/internal/pricing"
	"github.com/syft8/burnban/internal/proxy"
	"github.com/syft8/burnban/internal/store"
	"github.com/syft8/burnban/internal/web"
)

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 4141, "listen port")
	host := fs.String("host", "127.0.0.1", "bind address; anything non-loopback requires BURNBAN_TOKEN (team mode)")
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)

	token := os.Getenv("BURNBAN_TOKEN")
	if *host != "127.0.0.1" && *host != "localhost" && *host != "::1" && token == "" {
		return fmt.Errorf("refusing to bind %s without BURNBAN_TOKEN set — team mode fails closed", *host)
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	prices, err := pricing.Load()
	if err != nil {
		return err
	}
	p, err := proxy.New(s, prices, proxy.DefaultUpstreams())
	if err != nil {
		return err
	}

	addr := fmt.Sprintf("%s:%d", *host, *port)
	base := "http://" + addr
	authState := "open (localhost only)"
	if token != "" {
		authState = "token required (BURNBAN_TOKEN)"
	}

	capState, _ := s.GetSetting(budget.KeyDailyCapUSD)
	if capState == "" {
		capState = "none — set one: burnban cap --daily 10"
	} else {
		capState = "$" + capState + "/day"
	}
	banState := ""
	if ban, _ := s.GetSetting(budget.KeyBanActive); ban == "1" {
		banState = "\n   🚫 BURN BAN IN EFFECT — lift with: burnban lift\n"
	}

	fmt.Printf(`🔥 burnban %s — the meter is running

   dashboard   %s

   point your agents here:
     anthropic   ANTHROPIC_BASE_URL=%s/anthropic
     openai      OPENAI_BASE_URL=%s/openai/v1
     xai         %s/xai/v1

   db    %s
   cap   %s
   auth  %s
%s
   watch it live:  burnban top  (or open the dashboard)

`, version, base, base, base, base, *dbPath, capState, authState, banState)

	mux := http.NewServeMux()
	mux.Handle("/", p.Handler())
	web.Register(mux, s, version)
	srv := &http.Server{Addr: addr, Handler: web.WithAuth(token, mux)}
	return srv.ListenAndServe()
}
