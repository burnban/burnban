package web

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"time"

	"github.com/burnban/burnban/internal/optimize"
	"github.com/burnban/burnban/internal/store"
)

func registerQualityAPI(mux *http.ServeMux, s *store.Store) {
	mux.HandleFunc("POST /api/quality-scores", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			http.Error(w, "burnban: Content-Type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, optimize.MaxQualityPayloadBytes)
		records, err := optimize.ParseQualityDocument(r.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "burnban: external quality payload is too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "burnban: invalid external quality evidence: "+err.Error(), http.StatusBadRequest)
			return
		}
		result, err := s.ImportQualityScores(records, time.Now().UTC())
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, store.ErrQualityInvalid) {
				status = http.StatusBadRequest
			} else if errors.Is(err, store.ErrQualityConflict) || errors.Is(err, store.ErrQualityCaseConflict) {
				status = http.StatusConflict
			}
			http.Error(w, "burnban: quality import failed: "+err.Error(), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = jsonResponse(w, result)
	})
}

func jsonResponse(w http.ResponseWriter, value any) error {
	// Kept tiny and private so the ingestion endpoint cannot accidentally grow
	// into a generic tracing API.
	return json.NewEncoder(w).Encode(value)
}
