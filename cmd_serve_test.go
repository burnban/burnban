package main

import (
	"strings"
	"testing"
)

// Route names become ServeMux patterns; anything that could panic the mux
// or register a wildcard must die at flag parsing, not at startup.
func TestUpstreamFlagRejectsUnsafeNames(t *testing.T) {
	for _, bad := range []string{
		"x{y=http://localhost:1", // unclosed brace: mux panics
		"{x}=http://localhost:1", // wildcard: swallows arbitrary paths
		"a/b=http://localhost:1", // slash: nested route
		"a b=http://localhost:1", // space
		"api=http://localhost:1", // reserved for the dashboard
		"metrics=http://localhost:1",
		"health=http://localhost:1",
		"=http://localhost:1",
		"noequals",
		"groq=api.groq.com", // missing scheme
	} {
		u := upstreamFlags{}
		if err := u.Set(bad); err == nil {
			t.Errorf("Set(%q) accepted an unsafe upstream", bad)
		}
	}
}

func TestUpstreamFlagShapes(t *testing.T) {
	u := upstreamFlags{}
	if err := u.Set("groq=https://api.groq.com/openai"); err != nil {
		t.Fatal(err)
	}
	if got := u["groq"]; got.URL != "https://api.groq.com/openai" || got.Shape != "" {
		t.Fatalf("groq = %+v, want unspecified shape (meters OpenAI by default)", got)
	}
	if err := u.Set("corp=anthropic:https://llm.corp.internal"); err != nil {
		t.Fatal(err)
	}
	if got := u["corp"]; got.URL != "https://llm.corp.internal" || got.Shape != "anthropic" {
		t.Fatalf("corp = %+v", got)
	}
	// "https" is not a shape: a bare url with a scheme must parse as a url.
	if err := u.Set("mistral=https://api.mistral.ai"); err != nil {
		t.Fatal(err)
	}
	if got := u["mistral"]; got.URL != "https://api.mistral.ai" {
		t.Fatalf("mistral = %+v", got)
	}
	if s := u.String(); !strings.Contains(s, "corp=") || !strings.Contains(s, "groq=") {
		t.Fatalf("String() = %q", s)
	}
}

func TestLoopbackAndURLRedaction(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "127.42.0.9", "::1", "[::1]"} {
		if !isLoopbackHost(host) {
			t.Errorf("%q should be loopback", host)
		}
	}
	for _, host := range []string{"0.0.0.0", "192.168.1.4", "example.com", ""} {
		if isLoopbackHost(host) {
			t.Errorf("%q should not be loopback", host)
		}
	}
	got := redactURL("https://user:password@example.com/v1?api_key=secret")
	if strings.Contains(got, "user") || strings.Contains(got, "password") || strings.Contains(got, "secret") {
		t.Fatalf("redactURL leaked credentials: %q", got)
	}
}
