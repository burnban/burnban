package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/burnban/burnban/internal/optimize"
	"github.com/burnban/burnban/internal/store"
)

const qualityAPIBody = `{
  "schema":"burnban.external-quality/v1",
  "source":"external-eval",
  "metric":"success",
  "cohort":"release-1",
  "direction":"higher_is_better",
  "scores":[{
    "id":"score-1",
    "observed_at":"2026-07-12T12:00:00Z",
    "model":"model-a",
    "case_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "score":"0.9"
  }]
}`

func TestQualityAPIRequiresAuthAndProvidesIdempotentImmutableIngestion(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mux := http.NewServeMux()
	registerQualityAPI(mux, s)
	handler := WithAuth("secret", mux)

	request := func(body, token, contentType string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/quality-scores", strings.NewReader(body))
		if token != "" {
			req.Header.Set("x-burnban-token", token)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	if response := request(qualityAPIBody, "", "application/json"); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", response.Code)
	}
	if response := request(qualityAPIBody, "secret", "text/plain"); response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("wrong content-type status = %d", response.Code)
	}
	if response := request(qualityAPIBody, "secret", "application/json"); response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"inserted":1`) {
		t.Fatalf("first import status=%d body=%q", response.Code, response.Body.String())
	}
	if response := request(qualityAPIBody, "secret", "application/json; charset=utf-8"); response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"replayed":1`) {
		t.Fatalf("replay status=%d body=%q", response.Code, response.Body.String())
	}
	conflict := strings.Replace(qualityAPIBody, `"score":"0.9"`, `"score":"0.8"`, 1)
	if response := request(conflict, "secret", "application/json"); response.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%q", response.Code, response.Body.String())
	}
	invalid := strings.Replace(qualityAPIBody, `"source":"external-eval"`, `"source":"=formula"`, 1)
	invalid = strings.Replace(invalid, `"id":"score-1"`, `"id":"score-2"`, 1)
	if response := request(invalid, "secret", "application/json"); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid evidence status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestQualityAPIRejectsDuplicateFieldsWithoutPersisting(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mux := http.NewServeMux()
	registerQualityAPI(mux, s)
	body := strings.Replace(qualityAPIBody, `"source":"external-eval",`, `"source":"external-eval","Source":"other",`, 1)
	req := httptest.NewRequest(http.MethodPost, "/api/quality-scores", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestQualityAPIEnforcesPayloadBound(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mux := http.NewServeMux()
	registerQualityAPI(mux, s)
	req := httptest.NewRequest(http.MethodPost, "/api/quality-scores", strings.NewReader(strings.Repeat(" ", optimize.MaxQualityPayloadBytes+1)))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, req)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}
