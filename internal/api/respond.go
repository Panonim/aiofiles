package api

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"

	"aiofiles/internal/presets"
)

// maxJSONBody caps ordinary (non-upload) request bodies.
const maxJSONBody = 1 << 20

// Validation failures carry field/reason so the UI can highlight the offending control.
type errorBody struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Field   string `json:"field,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"error":{"code":"encode_failed","message":"could not encode response"}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: errorBody{Code: code, Message: message}})
}

// A *presets.ValidationError keeps its field/reason; anything else is a generic 400.
func writeValidationError(w http.ResponseWriter, err error) {
	var ve *presets.ValidationError
	if errors.As(err, &ve) {
		writeJSON(w, http.StatusBadRequest, errorEnvelope{Error: errorBody{
			Code: "invalid_params", Field: ve.Field, Reason: ve.Reason,
		}})
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	// A JSON endpoint only accepts JSON: this is what kills the cross-origin
	// <form enctype="text/plain"> trick, whose body the decoder would swallow.
	// The frontend always sets the header, and so do the docs' curl examples.
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"Content-Type must be application/json")
		return false
	}

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		return false
	}
	return true
}
