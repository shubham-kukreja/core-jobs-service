package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/shubham-kukreja/core-jobs-service/internal/auth"
	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

// errorBody is the public error shape.
type errorBody struct {
	Error errorPayload `json:"error"`
}

type errorPayload struct {
	Code      apperr.Code     `json:"code"`
	Message   string          `json:"message"`
	Details   []apperr.Detail `json:"details,omitempty"`
	RequestID string          `json:"request_id"`
}

var codeStatus = map[apperr.Code]int{
	apperr.Unauthorized:        http.StatusUnauthorized,
	apperr.Forbidden:           http.StatusForbidden,
	apperr.NotFound:            http.StatusNotFound,
	apperr.ValidationFailed:    http.StatusBadRequest,
	apperr.InvalidParams:       http.StatusBadRequest,
	apperr.NotUserTriggerable:  http.StatusBadRequest,
	apperr.Conflict:            http.StatusConflict,
	apperr.QueueFull:           http.StatusConflict,
	apperr.NotAvailable:        http.StatusConflict,
	apperr.IdempotencyMismatch: http.StatusUnprocessableEntity,
	apperr.RateLimited:         http.StatusTooManyRequests,
	apperr.PayloadTooLarge:     http.StatusRequestEntityTooLarge,
	apperr.UnsupportedMedia:    http.StatusUnsupportedMediaType,
	apperr.Internal:            http.StatusInternalServerError,
}

// toAppErr maps module sentinels to public errors. Anything unrecognised is
// internal: its cause is logged, never returned.
func toAppErr(err error) *apperr.Error {
	if e, ok := apperr.As(err); ok {
		return e
	}
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		return apperr.New(apperr.PayloadTooLarge, "Request body too large")
	case errors.Is(err, catalog.ErrNotFound), errors.Is(err, datapoints.ErrNotFound), errors.Is(err, auth.ErrNotFound):
		return apperr.New(apperr.NotFound, "Not found")
	case errors.Is(err, catalog.ErrCycle):
		return apperr.New(apperr.Conflict, "Derivation would create a dependency cycle")
	case errors.Is(err, datapoints.ErrAlreadyRetracted):
		return apperr.New(apperr.Conflict, "Datapoint is already retracted")
	case errors.Is(err, auth.ErrWeakPassword):
		return apperr.New(apperr.ValidationFailed, "Password does not meet the policy")
	}
	return apperr.Wrap(err, apperr.Internal, "Internal error")
}

// writeError writes err in the public shape and logs server-side causes.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	e := toAppErr(err)
	status, ok := codeStatus[e.Code]
	if !ok {
		status = http.StatusInternalServerError
	}
	rid := requestIDFrom(r.Context())
	if status >= 500 {
		s.log.ErrorContext(r.Context(), "request failed", "request_id", rid, "method", r.Method, "path", r.URL.Path, "err", err)
	}
	msg := e.Message
	if e.Code == apperr.Internal {
		msg = "Internal error" // never leak a wrapped message
	}
	writeJSON(w, status, errorBody{Error: errorPayload{Code: e.Code, Message: msg, Details: e.Details, RequestID: rid}})
}

// writeJSON encodes v. Encoding happens before headers are written so a
// marshal failure still yields a clean 500.
func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(v); err != nil {
		slog.Default().Error("encode response", "err", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"code":"internal","message":"Internal error","request_id":""}}`)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// decodeJSON strictly decodes exactly one JSON object from body: unknown
// fields, trailing data and a wrong content type are rejected.
func decodeJSON(r *http.Request, body []byte, v any) error {
	ct := r.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return apperr.New(apperr.UnsupportedMedia, "Content-Type must be application/json")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalidJSON(err)
	}
	if dec.More() {
		return apperr.New(apperr.ValidationFailed, "Request body must contain a single JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return apperr.New(apperr.ValidationFailed, "Request body must contain a single JSON object")
	}
	return nil
}

func invalidJSON(err error) error {
	var ute *json.UnmarshalTypeError
	var se *json.SyntaxError
	switch {
	case errors.As(err, &ute):
		return apperr.New(apperr.ValidationFailed, "Invalid field type", apperr.Detail{Field: ute.Field, Reason: "wrong_type"})
	case errors.As(err, &se), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return apperr.New(apperr.ValidationFailed, "Malformed JSON")
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return apperr.New(apperr.ValidationFailed, "Unknown field", apperr.Detail{Field: field, Reason: "unknown_field"})
	}
	return apperr.New(apperr.ValidationFailed, "Invalid request body")
}

// readBody reads the (already size-limited) body.
func readBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, apperr.New(apperr.ValidationFailed, "Request body required")
	}
	return b, nil
}

// decodeBody reads and strictly decodes the request body into v.
func decodeBody(r *http.Request, v any) error {
	b, err := readBody(r)
	if err != nil {
		return err
	}
	return decodeJSON(r, b, v)
}
