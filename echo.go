package middlemonitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

// responseWriterWrapper wraps the response writer to capture status codes and, for 5xx, the response body and handler location.
type responseWriterWrapper struct {
	http.ResponseWriter
	statusCode *int
	written    bool
	// bodyCapture: for 5xx we capture the first chunk to try to extract an "error" field from JSON
	bodyCapture *bytes.Buffer
	maxCapture  int
	// capturedFile/Line: when writing 5xx body, capture the handler's file/line from the stack (so UI shows main.go, not "handler")
	capturedFile string
	capturedLine int
}

func (w *responseWriterWrapper) WriteHeader(code int) {
	if !w.written {
		*w.statusCode = code
		w.written = true
		if code >= 500 && w.bodyCapture != nil {
			// will capture in Write
		}
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *responseWriterWrapper) Write(b []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	if w.bodyCapture != nil && *w.statusCode >= 500 && w.bodyCapture.Len() < w.maxCapture {
		remain := w.maxCapture - w.bodyCapture.Len()
		if len(b) > remain {
			w.bodyCapture.Write(b[:remain])
		} else {
			w.bodyCapture.Write(b)
		}
		// Capture handler location once (stack here is: Write -> Echo -> ... -> user handler)
		if w.capturedFile == "" {
			w.capturedFile, w.capturedLine = getHandlerLocation()
		}
	}
	return w.ResponseWriter.Write(b)
}

// EchoMiddleware returns an Echo middleware that automatically creates traces and logs with OpenTelemetry
// Usage: e.Use(middlemonitor.EchoMiddleware())
func EchoMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// Get client and config
			client := GetGlobalClient()
			if client == nil {
				// If not initialized, just call next without instrumentation
				return next(c)
			}

			cfg := GetGlobalConfig()
			tracer := client.GetTracer()

			// Extract context from request headers (W3C Trace Context)
			propagator := otel.GetTextMapPropagator()
			headerCarrier := propagation.HeaderCarrier(c.Request().Header)
			ctx := propagator.Extract(c.Request().Context(), headerCarrier)

			// Get route information
			route := c.Path()
			if route == "" {
				route = c.Request().URL.Path
			}
			method := c.Request().Method

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
						semconv.HTTPURLKey.String(c.Request().URL.String()),
					),
				)
				defer span.End()
			} else {
				// Still propagate context even if we don't sample
				ctx = trace.ContextWithSpan(ctx, trace.SpanFromContext(ctx))
			}

			// Store request info and Echo context so ReportExceptionWithContext can recover method/URL
			// and set KeyExceptionMessage (avoids duplicate submission with generic "HTTP 500").
			ctx = context.WithValue(ctx, requestContextKey{}, &requestInfo{Method: method, URL: c.Request().URL.String()})
			ctx = context.WithValue(ctx, frameworkContextKey{}, c)
			c.SetRequest(c.Request().WithContext(ctx))

			// Capture request body for error reporting (limited size)
			var requestBody []byte
			if c.Request().Body != nil {
				body, _ := io.ReadAll(c.Request().Body)
				if len(body) < 10000 {
					requestBody = body
				} else {
					requestBody = body[:10000]
				}
				c.Request().Body = io.NopCloser(bytes.NewBuffer(body))
			}

			// Capture panics
			var panicErr error
			defer func() {
				if r := recover(); r != nil {
					switch v := r.(type) {
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
						// So the panic is exported as a trace (backend + OpenSearch)
						reqCtx := c.Request().Context()
						_, panicSpan := tracer.Start(reqCtx, spanName,
							trace.WithAttributes(
								semconv.HTTPMethodKey.String(method),
								semconv.HTTPRouteKey.String(route),
								semconv.HTTPURLKey.String(c.Request().URL.String()),
								semconv.HTTPStatusCodeKey.Int(http.StatusInternalServerError),
								attribute.Bool("error", true),
								attribute.String("error.type", "panic"),
							),
						)
						panicSpan.RecordError(panicErr)
						panicSpan.SetStatus(codes.Error, panicErr.Error())
						panicSpan.End()
					}

					// Send to backend Errors API (application_errors) so it appears in Errors view.
					// Use first stack frame outside SDK/Echo so the reported file:line is the user's code (e.g. main.go), not echo.go.
					file, line := getPanicLocation()
					go submitApplicationError(cfg, "panic", panicErr.Error(), file, line, http.StatusInternalServerError, method, c.Request().URL.String(), requestBody)

					panic(r) // Re-panic to let Echo's Recover middleware handle it
				}
			}()

			// Wrap response writer to capture status code and, for 5xx, response body (to extract error message for Errors view)
			originalWriter := c.Response().Writer
			statusCode := 200
			wrapper := &responseWriterWrapper{
				ResponseWriter: originalWriter,
				statusCode:     &statusCode,
				bodyCapture:    bytes.NewBuffer(nil),
				maxCapture:     4096,
			}
			c.Response().Writer = wrapper

			// Execute handler
			err := next(c)

			// Get final status code
			finalStatus := *wrapper.statusCode

			// Auto-hide 5xx: if handler returned an error and no response was written yet, send 500 with empty body
			// and store the message for Middle Monitor (so you can just "return err" without c.Set every time).
			if err != nil && !wrapper.written {
				c.Set(KeyExceptionMessage, err.Error())
				_ = c.JSON(http.StatusInternalServerError, map[string]interface{}{})
				err = nil
				finalStatus = *wrapper.statusCode
			}

			// Determine if there's an error
			hasError := err != nil || finalStatus >= 400

			// Update span with status code
			if span != nil {
				span.SetAttributes(
					semconv.HTTPStatusCodeKey.Int(finalStatus),
					attribute.Bool("error", hasError),
				)

				if hasError {
					if err != nil {
						span.RecordError(err)
						span.SetStatus(codes.Error, err.Error())
					} else {
						span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", finalStatus))
					}
				} else {
					span.SetStatus(codes.Ok, "")
				}
			}

			// Only report server errors (5xx) and panics to Middle Monitor; 4xx (401, 404, etc.) are customer-facing.
			isServerError := finalStatus >= 500

			// For routes not sampled initially (e.g. /health), create a span only for 5xx
			if !shouldSample && hasError && isServerError && cfg.ShouldSampleTrace(route, true) {
				reqCtx := c.Request().Context()
				_, errorSpan := tracer.Start(reqCtx, spanName,
					trace.WithAttributes(
						semconv.HTTPMethodKey.String(method),
						semconv.HTTPRouteKey.String(route),
						semconv.HTTPURLKey.String(c.Request().URL.String()),
						semconv.HTTPStatusCodeKey.Int(finalStatus),
						attribute.Bool("error", true),
					),
				)
				if err != nil {
					errorSpan.RecordError(err)
					errorSpan.SetStatus(codes.Error, err.Error())
				} else {
					errorSpan.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", finalStatus))
				}
				errorSpan.End()
			}

			// Send only 5xx (and panics) to the Errors view; 4xx are not reported
			if hasError && isServerError {
				msg := getMessageForException(err, finalStatus, wrapper.bodyCapture)
				// If handler returned 500 with empty/generic body but set KeyExceptionMessage, use that so Middle Monitor still has the real message
				if isGenericExceptionMessage(msg) {
					if v := c.Get(KeyExceptionMessage); v != nil {
						if s, ok := v.(string); ok && s != "" {
							msg = s
						}
					}
				}
				file, line := wrapper.capturedFile, wrapper.capturedLine
				if file == "" {
					file, line = "handler", 0
				}
				go submitApplicationError(cfg, "http", msg, file, line, finalStatus, method, c.Request().URL.String(), requestBody)
			}

			return err
		}
	}
}

// appErrorPayload is the JSON body for POST /api/v1/errors (application_errors table)
type appErrorPayload struct {
	Name       string    `json:"name"`
	Message    string    `json:"message"`
	File       string    `json:"file"`
	Line       int       `json:"line"`
	Timestamp  time.Time `json:"timestamp"`
	Service    string    `json:"service"`
	HTTPMethod *string   `json:"http_method,omitempty"`
	HTTPURL    *string   `json:"http_url,omitempty"`
	HTTPBody   *string   `json:"http_body,omitempty"`
}

// getPanicLocation returns the first (file, line) from the current stack that is not inside
// the SDK or the Echo/Gin framework, so reported errors point to the user's code (e.g. main.go).
func getPanicLocation() (file string, line int) {
	return getHandlerLocationFrom(1)
}

// getHandlerLocation is for use when the stack is from Write() (response write): skips our wrapper + Echo/Gin framework.
// Call with no args from Write(); the first frame is Write (echo.go), then Echo/Gin, then user handler.
func getHandlerLocation() (file string, line int) {
	return getHandlerLocationFrom(1)
}

// getHandlerLocationFrom walks the stack starting at skip and returns the first (file, line) not in SDK, framework, or stdlib.
func getHandlerLocationFrom(initialSkip int) (file string, line int) {
	const maxFrames = 25
	goroot := runtime.GOROOT()
	for skip := initialSkip; skip < maxFrames; skip++ {
		pc, path, ln, ok := runtime.Caller(skip)
		if !ok {
			break
		}
		if path == "" {
			continue
		}
		base := filepath.Base(path)
		if base == "echo.go" || base == "gin.go" {
			continue
		}
		norm := strings.ReplaceAll(filepath.ToSlash(path), "\\", "/")
		if strings.Contains(norm, "middlemonitor") || strings.Contains(norm, "sdk-go") || strings.Contains(norm, "middle-monitor/sdks/go") {
			continue
		}
		if strings.Contains(norm, "labstack/echo") || strings.Contains(norm, "gin-gonic/gin") {
			continue
		}
		// Skip Go standard library (e.g. encoding/json/stream.go, net/http) so we reach user code (main.go)
		if goroot != "" {
			if rel, err := filepath.Rel(goroot, path); err == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
				continue
			}
		}
		if fn := runtime.FuncForPC(pc); fn != nil {
			if strings.HasPrefix(fn.Name(), "runtime.") {
				continue
			}
		}
		return path, ln
	}
	return "", 0
}

// isGenericExceptionMessage returns true when the message is just "HTTP 5xx" with no detail (e.g. handler returned empty body).
func isGenericExceptionMessage(msg string) bool {
	return strings.HasPrefix(msg, "HTTP ") && len(msg) <= 12
}

// getMessageForException returns the error message to send to the Errors view: from err, or from 5xx response body "error" field, or "HTTP 500".
func getMessageForException(err error, statusCode int, bodyCapture *bytes.Buffer) string {
	if err != nil {
		return err.Error()
	}
	if bodyCapture != nil && bodyCapture.Len() > 0 {
		var m map[string]interface{}
		if json.Unmarshal(bodyCapture.Bytes(), &m) == nil {
			if s, _ := m["error"].(string); s != "" {
				return s
			}
		}
	}
	return fmt.Sprintf("HTTP %d", statusCode)
}

// ReportException sends the given message to Middle Monitor (e.g. from a background job).
// File/line are taken from the call stack. No HTTP code or URL — use ReportExceptionWithContext when in a request.
func ReportException(message string) {
	reportException(message, 0, "", "")
}

// ReportExceptionWithContext reports an error to Middle Monitor. Pass the request context (e.g. c.Request().Context()) when
// inside an HTTP handler: method, URL and status 500 are then recovered automatically from the context set by the middleware.
// Outside a request, pass nil: only message and file/line from the stack are sent.
//
// Example (Echo):
//
//	if err != nil {
//	    middlemonitor.ReportExceptionWithContext(c.Request().Context(), err)
//	    return c.JSON(http.StatusInternalServerError, map[string]interface{}{})
//	}
//
// Example (Gin): middlemonitor.ReportExceptionWithContext(c.Request.Context(), err)
func ReportExceptionWithContext(ctx context.Context, err error) {
	if err == nil {
		return
	}
	message := err.Error()
	// When in an Echo/Gin handler: set KeyExceptionMessage so the middleware uses this message
	// when it submits (single submission with correct message; avoids duplicate "HTTP 500" from empty body).
	if ctx != nil {
		if fc := ctx.Value(frameworkContextKey{}); fc != nil {
			type setter interface{ Set(string, interface{}) }
			if s, ok := fc.(setter); ok {
				s.Set(KeyExceptionMessage, message)
				return
			}
		}
	}
	statusCode := 0
	var method, url string
	if ctx != nil {
		if ri := ctx.Value(requestContextKey{}); ri != nil {
			if r, ok := ri.(*requestInfo); ok {
				statusCode = 500
				method, url = r.Method, r.URL
			}
		}
	}
	reportException(message, statusCode, method, url)
}

func reportException(message string, statusCode int, method, url string) {
	if statusCode > 0 && statusCode < 500 {
		return
	}
	cfg := GetGlobalConfig()
	if cfg == nil || cfg.Endpoint == "" {
		return
	}
	file, line := getHandlerLocationFrom(2)
	if file == "" {
		file, line = "handler", 0
	}
	go submitApplicationError(cfg, "http", message, file, line, statusCode, method, url, nil)
}

// ReportExceptionFromEcho reports an exception with only the message. Method, URL and file/line are read from the Echo context and the call stack; the HTTP code is assumed 500 (server error).
//
// Example:
//
//	if err != nil {
//	    middlemonitor.ReportExceptionFromEcho(c, err.Error())
//	    return c.JSON(http.StatusInternalServerError, map[string]interface{}{})
//	}
func ReportExceptionFromEcho(c echo.Context, message string) {
	if c != nil && c.Request() != nil {
		reportException(message, 500, c.Request().Method, c.Request().URL.String())
	} else {
		reportException(message, 0, "", "")
	}
}

// submitApplicationError sends the error to the backend Errors API so it appears in the Errors view.
func submitApplicationError(cfg *Config, name, message, file string, line, statusCode int, method, url string, requestBody []byte) {
	if cfg == nil || cfg.Endpoint == "" {
		return
	}
	payload := appErrorPayload{
		Name:      name,
		Message:   message,
		File:      file,
		Line:      line,
		Timestamp: time.Now().UTC(),
		Service:   cfg.Service,
	}
	if method != "" {
		payload.HTTPMethod = &method
	}
	if url != "" {
		payload.HTTPURL = &url
	}
	if len(requestBody) > 0 {
		bodyStr := string(requestBody)
		if len(bodyStr) > 2000 {
			bodyStr = bodyStr[:2000] + "..."
		}
		payload.HTTPBody = &bodyStr
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	baseURL := cfg.Endpoint
	if baseURL != "" && !strings.Contains(cfg.Endpoint, "://") {
		baseURL = "http://" + baseURL
	}
	req, err := http.NewRequestWithContext(context.Background(), "POST", baseURL+"/api/v1/errors", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Error("failed to submit error to backend", "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		slog.Warn("backend errors API returned error", "status", resp.StatusCode)
	}
}
