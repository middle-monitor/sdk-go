package middlemonitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

// httpExceptionStore lets ReportExceptionWithContext hand the real error message to
// HTTPMiddleware the same way Echo/Gin contexts do (it satisfies the same Set interface).
type httpExceptionStore struct {
	mu  sync.Mutex
	msg string
}

func (s *httpExceptionStore) Set(key string, value interface{}) {
	if key != KeyExceptionMessage {
		return
	}
	v, ok := value.(string)
	if !ok {
		return
	}
	s.mu.Lock()
	s.msg = v
	s.mu.Unlock()
}

func (s *httpExceptionStore) message() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msg
}

// HTTPMiddleware instruments any net/http handler: traces with OpenTelemetry, captures
// panics, and reports 5xx responses to the Errors view. It plugs into http.ServeMux,
// gorilla/mux (r.Use(middlemonitor.HTTPMiddleware)), chi, or a raw http.Handler chain.
//
// net/http exposes no route template, so the raw URL path is used as the route for
// span names and sampling rules.
//
// Usage: http.ListenAndServe(addr, middlemonitor.HTTPMiddleware(mux))
func HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client := GetGlobalClient()
		if client == nil {
			// If not initialized, just call next without instrumentation
			next.ServeHTTP(w, r)
			return
		}

		cfg := GetGlobalConfig()
		tracer := client.GetTracer()

		// Extract context from request headers (W3C Trace Context)
		propagator := otel.GetTextMapPropagator()
		ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))

		route := r.URL.Path
		method := r.Method

		// Decide if we should sample this trace
		shouldSample := cfg.ShouldSampleTrace(route, false) // We don't know if there's an error yet

		// Start span
		spanName := fmt.Sprintf("%s %s", method, route)
		var span trace.Span
		if shouldSample {
			ctx, span = tracer.Start(ctx, spanName,
				trace.WithAttributes(
					semconv.HTTPMethodKey.String(method),
					semconv.HTTPRouteKey.String(route),
					semconv.HTTPURLKey.String(r.URL.String()),
				),
			)
			defer span.End()
		} else {
			// Still propagate context even if we don't sample
			ctx = trace.ContextWithSpan(ctx, trace.SpanFromContext(ctx))
		}

		// Store request info and the exception store so ReportExceptionWithContext can recover
		// method/URL and set KeyExceptionMessage (single submission with the real message).
		store := &httpExceptionStore{}
		ctx = context.WithValue(ctx, requestContextKey{}, &requestInfo{Method: method, URL: r.URL.String()})
		ctx = context.WithValue(ctx, frameworkContextKey{}, store)
		r = r.WithContext(ctx)

		// Capture request body for error reporting (limited size)
		var requestBody []byte
		if r.Body != nil {
			body, _ := io.ReadAll(r.Body)
			if len(body) < 10000 {
				requestBody = body
			} else {
				requestBody = body[:10000]
			}
			r.Body = io.NopCloser(bytes.NewBuffer(body))
		}

		// Capture panics
		defer func() {
			if rec := recover(); rec != nil {
				var panicErr error
				switch v := rec.(type) {
				case error:
					panicErr = v
				case string:
					panicErr = errors.New(v)
				default:
					panicErr = fmt.Errorf("%v", v)
				}

				// Record panic in span (or create an error span if we didn't sample this route)
				if span != nil {
					span.RecordError(panicErr)
					span.SetStatus(codes.Error, panicErr.Error())
					span.SetAttributes(
						attribute.Bool("error", true),
						attribute.String("error.type", "panic"),
					)
				} else {
					_, panicSpan := tracer.Start(r.Context(), spanName,
						trace.WithAttributes(
							semconv.HTTPMethodKey.String(method),
							semconv.HTTPRouteKey.String(route),
							semconv.HTTPURLKey.String(r.URL.String()),
							semconv.HTTPStatusCodeKey.Int(http.StatusInternalServerError),
							attribute.Bool("error", true),
							attribute.String("error.type", "panic"),
						),
					)
					panicSpan.RecordError(panicErr)
					panicSpan.SetStatus(codes.Error, panicErr.Error())
					panicSpan.End()
				}

				file, line := getPanicLocation()
				go submitApplicationError(cfg, "panic", panicErr.Error(), file, line, http.StatusInternalServerError, method, r.URL.String(), requestBody)

				panic(rec) // Re-panic to let the server's recovery handle it
			}
		}()

		// Wrap response writer to capture status code and, for 5xx, response body (to extract error message for Errors view)
		statusCode := 200
		wrapper := &responseWriterWrapper{
			ResponseWriter: w,
			statusCode:     &statusCode,
			bodyCapture:    bytes.NewBuffer(nil),
			maxCapture:     4096,
		}

		// Execute handler
		next.ServeHTTP(wrapper, r)

		// Get final status code
		finalStatus := *wrapper.statusCode

		// Determine if there's an error
		hasError := finalStatus >= 400
		// Only report server errors (5xx) to Middle Monitor; 4xx are customer-facing
		isServerError := finalStatus >= 500

		// Update span with status code
		if span != nil {
			span.SetAttributes(
				semconv.HTTPStatusCodeKey.Int(finalStatus),
				attribute.Bool("error", hasError),
			)

			if hasError {
				span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", finalStatus))
			} else {
				span.SetStatus(codes.Ok, "")
			}
		}

		// For routes not sampled initially (e.g. /health), create a span only for 5xx
		if !shouldSample && isServerError && cfg.ShouldSampleTrace(route, true) {
			_, errorSpan := tracer.Start(r.Context(), spanName,
				trace.WithAttributes(
					semconv.HTTPMethodKey.String(method),
					semconv.HTTPRouteKey.String(route),
					semconv.HTTPURLKey.String(r.URL.String()),
					semconv.HTTPStatusCodeKey.Int(finalStatus),
					attribute.Bool("error", true),
				),
			)
			errorSpan.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", finalStatus))
			errorSpan.End()
		}

		// Send only server errors (5xx) to the Errors view; 4xx are not reported
		if isServerError {
			msg := getMessageForException(nil, finalStatus, wrapper.bodyCapture)
			// If handler wrote a 500 with empty/generic body but called ReportExceptionWithContext,
			// use the stored message so Middle Monitor still has the real cause
			if isGenericExceptionMessage(msg) {
				if s := store.message(); s != "" {
					msg = s
				}
			}
			file, line := wrapper.capturedFile, wrapper.capturedLine
			if file == "" {
				file, line = "handler", 0
			}
			go submitApplicationError(cfg, "http", msg, file, line, finalStatus, method, r.URL.String(), requestBody)
		}
	})
}
