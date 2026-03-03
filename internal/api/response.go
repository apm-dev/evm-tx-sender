package api

import (
	"encoding/json"
	"net/http"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
)

type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, err *domain.AppError) {
	writeJSON(w, err.HTTP, ErrorResponse{
		Code:    string(err.Code),
		Message: err.Message,
	})
}
