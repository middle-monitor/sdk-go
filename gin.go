package middlemonitor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

// ginResponseWriter wraps gin's ResponseWriter to capture status codes and, for 5xx, the response body and handler location.
type ginResponseWriter struct {
	gin.ResponseWriter
	statusCode   *int
	written      bool
	bodyCapture  *bytes.Buffer
	maxCapture   int
	capturedFile string
	capturedLine int
}

func (w *ginResponseWriter) WriteHeader(code int) {
	if !w.written {
		*w.statusCode = code
		w.written = true
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *ginResponseWriter) Write(b []byte) (int, error) {
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
		if w.capturedFile == "" {
			w.capturedFile, w.capturedLine = getHandlerLocation()
		}
	}
	return w.ResponseWriter.Write(b)
}

// GinMiddleware returns a Gin middleware that automatically creates traces and logs with OpenTelemetry
// Usage: r.Use(middlemonitor.GinMiddleware())
func GinMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Get client and config
		client := GetGlobalClient()
		if client == nil {
			// If not initialized, just call next without instrumentation
			c.Next()
			return
		}

		cfg := GetGlobalConfig()
		tracer := client.GetTracer()

		// Extract context from request headers (W3C Trace Context)
		propagator := otel.GetTextMapPropagator()
		headerCarrier := propagation.HeaderCarrier(c.Request.Header)
		ctx := propagator.Extract(c.Request.Context(), headerCarrier)

		// Get route information
		route := c.FullPath()
		if route == "" {
			route = c.Request.URL.Path
		}
		method := c.Request.Method

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
					semconv.HTTPURLKey.String(c.Request.URL.String()),
				),
			)
			defer span.End()
		} else {
			// Still propagate context even if we don't sample
			ctx = trace.ContextWithSpan(ctx, trace.SpanFromContext(ctx))
		}

		// Store request info and Gin context so ReportExceptionWithContext can set KeyExceptionMessage
		ctx = context.WithValue(ctx, requestContextKey{}, &requestInfo{Method: method, URL: c.Request.URL.String()})
		ctx = context.WithValue(ctx, frameworkContextKey{}, c)
		c.Request = c.Request.WithContext(ctx)

		// Capture request body for error reporting (limited size)
		var requestBody []byte
		if c.Request.Body != nil {
			body, _ := io.ReadAll(c.Request.Body)
			if len(body) < 10000 {
				requestBody = body
			} else {
				requestBody = body[:10000]
			}
			c.Request.Body = io.NopCloser(bytes.NewBuffer(body))
		}

		// Wrap response writer to capture status code and, for 5xx, response body (to extract error message for Errors view)
		originalWriter := c.Writer
		statusCode := 200
		wrapper := &ginResponseWriter{
			ResponseWriter: originalWriter,
			statusCode:     &statusCode,
			bodyCapture:    bytes.NewBuffer(nil),
			maxCapture:     4096,
		}
		c.Writer = wrapper

		// Capture panics
		defer func() {
			if r := recover(); r != nil {
				var panicErr error
				switch v := r.(type) {
				case error:
					panicErr = v
				case string:
					panicErr = fmt.Errorf("%s", v)
				default:
					panicErr = fmt.Errorf("%v", v)
				}

				// Record panic in span
				if span != nil {
					span.RecordError(panicErr)
					span.SetStatus(codes.Error, panicErr.Error())
					span.SetAttributes(
						attribute.Bool("error", true),
						attribute.String("error.type", "panic"),
					)
				}

				panic(r) // Re-panic to let Gin's recovery handle it
			}
		}()

		// Execute handler
		c.Next()

		// Get final status code
		finalStatus := *wrapper.statusCode

		// Check for errors in Gin context
		errs := c.Errors
		var err error
		if len(errs) > 0 {
			// Get the last error
			err = errs.Last().Err
		}

		// Determine if there's an error
		hasError := err != nil || finalStatus >= 400
		// Only report server errors (5xx) to Middle Monitor; 4xx are customer-facing
		isServerError := finalStatus >= 500

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

		// Send only server errors (5xx) to the Errors view (message from err or from response body "error" field)
		if hasError && isServerError {
			msg := getMessageForException(err, finalStatus, wrapper.bodyCapture)
			if isGenericExceptionMessage(msg) {
				if v, ok := c.Get(KeyExceptionMessage); ok {
					if s, ok := v.(string); ok && s != "" {
						msg = s
					}
				}
			}
			file, line := wrapper.capturedFile, wrapper.capturedLine
			if file == "" {
				file, line = "handler", 0
			}
			go submitApplicationError(cfg, "http", msg, file, line, finalStatus, method, c.Request.URL.String(), requestBody)
		}
	}
}

// ReportExceptionFromGin reports an exception with only the message. Method, URL and file/line are read from the Gin context and the call stack; the HTTP code is assumed 500 (server error).
//
// Example:
//
//	if err != nil {
//	    middlemonitor.ReportExceptionFromGin(c, err.Error())
//	    c.JSON(http.StatusInternalServerError, map[string]interface{}{})
//	    return
//	}
func ReportExceptionFromGin(c *gin.Context, message string) {
	if c != nil && c.Request != nil {
		reportException(message, 500, c.Request.Method, c.Request.URL.String())
	} else {
		reportException(message, 0, "", "")
	}
}
