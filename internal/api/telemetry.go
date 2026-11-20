package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// HTTPMetrics records one observation per request; telemetry.Prometheus
// implements it. route is the router pattern, never the raw path.
type HTTPMetrics interface {
	ObserveHTTP(method, route string, code int, d time.Duration)
}

type nopHTTPMetrics struct{}

func (nopHTTPMetrics) ObserveHTTP(string, string, int, time.Duration) {}

var tracer = otel.Tracer("github.com/shubham-kukreja/core-jobs-service/internal/api")

// instrument opens a server span (continuing an inbound W3C traceparent)
// and records metrics once the router has resolved the route pattern.
// Spans and labels carry the method, route and status only.
func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tracer.Start(ctx, r.Method, trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.request.method", r.Method)))
		defer span.End()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r.WithContext(ctx))

		route := "unmatched"
		if rc := chi.RouteContext(ctx); rc != nil {
			// No API route ends in a wildcard; one here is the not-found catch-all.
			if p := rc.RoutePattern(); p != "" && !strings.HasSuffix(p, "*") {
				route = p
			}
		}
		span.SetName(r.Method + " " + route)
		span.SetAttributes(attribute.String("http.route", route), attribute.Int("http.response.status_code", rec.status))
		if rec.status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(rec.status))
		}
		s.metrics.ObserveHTTP(r.Method, route, rec.status, time.Since(start))
	})
}
