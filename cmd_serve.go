package main

import (
	"flag"
	"fmt"
	"net/http"

	"github.com/syft8/burnban/internal/budget"
	"github.com/syft8/burnban/internal/pricing"
	"github.com/syft8/burnban/internal/proxy"
	"github.com/syft8/burnban/internal/store"
)

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 4141, "listen port (binds to localhost only)")
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)

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

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	base := "http://" + addr

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

   point your agents here:
     anthropic   ANTHROPIC_BASE_URL=%s/anthropic
     openai      OPENAI_BASE_URL=%s/openai/v1
     xai         %s/xai/v1

   db    %s
   cap   %s
%s
   watch it live:  burnban top

`, version, base, base, base, *dbPath, capState, banState)

	srv := &http.Server{Addr: addr, Handler: p.Handler()}
	return srv.ListenAndServe()
}
