package middlemonitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// KeyExceptionMessage is the context key for storing the real error message when returning a 500
// without exposing details to the client. The middleware reads this value and forwards it to
// Middle Monitor so the Errors view shows the actual cause rather than a generic "HTTP 500".
//
// Echo: c.Set(middlemonitor.KeyExceptionMessage, err.Error()); return c.JSON(500, map[string]any{})
// Gin:  c.Set(middlemonitor.KeyExceptionMessage, err.Error()); c.JSON(500, map[string]any{})
const KeyExceptionMessage = "middlemonitor_exception_message"

// requestInfo is stored in the request context by the middleware so ReportException(ctx, err) can recover method and URL.
type requestInfo struct {
	Method string
	URL    string
}

type requestContextKey struct{}

// frameworkContextKey stores Echo or Gin context so ReportExceptionWithContext can set KeyExceptionMessage
// when called from an HTTP handler (avoids duplicate submission and ensures correct message).
type frameworkContextKey struct{}

// NewClient creates a client. Prefer Init + GetGlobalClient for most cases.
func NewClient(apiURL, service string) *Client {
	cfg := NewConfig(apiURL, service, "")
	client, err := NewClientWithConfig(cfg)
	if err != nil {
		slog.Error("failed to create client", "error", err)
		return nil
	}
	return client
}

// SetToken sets the auth token.
func (c *Client) SetToken(token string) {
	if c.config != nil {
		c.config.Token = token
	}
}

// ReportError reports an error to Middle-Monitor using OpenTelemetry.
func (c *Client) ReportError(err error) error {
	return c.ReportErrorWithDetails(err, "", 0)
}

// ReportErrorWithDetails reports an error with custom file and line using OpenTelemetry.
func (c *Client) ReportErrorWithDetails(err error, file string, line int) error {
	if err == nil {
		return nil
	}

	if c == nil || c.tracer == nil {
		return ErrNotInitialized
	}

	if file == "" {
		_, file, line, _ = runtime.Caller(2)
	}

	ctx := context.Background()
	_, span := c.tracer.Start(ctx, "error.report",
		trace.WithAttributes(
			attribute.String("error.message", err.Error()),
			attribute.String("error.file", file),
			attribute.Int("error.line", line),
			attribute.String("service.name", c.config.Service),
			attribute.Bool("error", true),
		),
	)
	defer span.End()

	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())

	return nil
}

// ReportCustomError reports a named error with details.
func (c *Client) ReportCustomError(name, message, file string, line int) error {
	if c == nil || c.tracer == nil {
		return ErrNotInitialized
	}

	_, span := c.tracer.Start(context.Background(), fmt.Sprintf("error.%s", name),
		trace.WithAttributes(
			attribute.String("error.name", name),
			attribute.String("error.message", message),
			attribute.String("error.file", file),
			attribute.Int("error.line", line),
			attribute.String("service.name", c.config.Service),
			attribute.Bool("error", true),
		),
	)
	defer span.End()

	err := fmt.Errorf("%s: %s", name, message)
	span.RecordError(err)
	span.SetStatus(codes.Error, message)

	return nil
}

// ReportCustomErrorWithHTTP reports a named error with HTTP context.
func (c *Client) ReportCustomErrorWithHTTP(name, message, file string, line int, httpMethod, httpURL, httpHeaders, httpBody string) error {
	if c == nil || c.tracer == nil {
		return ErrNotInitialized
	}

	attrs := []attribute.KeyValue{
		attribute.String("error.name", name),
		attribute.String("error.message", message),
		attribute.String("error.file", file),
		attribute.Int("error.line", line),
		attribute.String("service.name", c.config.Service),
		attribute.Bool("error", true),
	}

	if httpMethod != "" {
		attrs = append(attrs, attribute.String("http.method", httpMethod))
	}
	if httpURL != "" {
		attrs = append(attrs, attribute.String("http.url", httpURL))
	}

	_, span := c.tracer.Start(context.Background(), fmt.Sprintf("error.%s", name),
		trace.WithAttributes(attrs...),
	)
	defer span.End()

	err := fmt.Errorf("%s: %s", name, message)
	span.RecordError(err)
	span.SetStatus(codes.Error, message)

	return nil
}

// SubmitApplicationError reports an error to the Errors view (POST /api/v1/errors).
func (c *Client) SubmitApplicationError(name, message, file string, line, statusCode int, httpMethod, httpURL, requestBody string) {
	if c == nil || c.config == nil {
		return
	}
	var body []byte
	if requestBody != "" {
		body = []byte(requestBody)
	}
	submitApplicationError(c.config, name, message, file, line, statusCode, httpMethod, httpURL, body)
}

// CapturePanic captures a panic and reports it to Middle-Monitor
// Usage: defer client.CapturePanic()
func (c *Client) CapturePanic() {
	if r := recover(); r != nil {
		var err error
		switch v := r.(type) {
		case error:
			err = v
		case string:
			err = errors.New(v)
		default:
			err = fmt.Errorf("%v", v)
		}

		_, file, line, _ := runtime.Caller(3)
		c.ReportErrorWithDetails(err, file, line)
		panic(r) // Re-panic after reporting
	}
}

// ReportError reports an error using the global client.
func ReportError(err error) {
	client := GetGlobalClient()
	if client != nil {
		client.ReportError(err)
	}
}

// ReportErrorWithDetails reports an error with file and line using the global client.
func ReportErrorWithDetails(err error, file string, line int) {
	client := GetGlobalClient()
	if client != nil {
		client.ReportErrorWithDetails(err, file, line)
	}
}

// SubmitApplicationError reports an error to the Errors view using the global client.
func SubmitApplicationError(name, message, file string, line, statusCode int, httpMethod, httpURL, requestBody string) {
	client := GetGlobalClient()
	if client != nil {
		client.SubmitApplicationError(name, message, file, line, statusCode, httpMethod, httpURL, requestBody)
	}
}

// CapturePanicGlobal captures a panic using the global client.
// Usage: defer middlemonitor.CapturePanicGlobal()
func CapturePanicGlobal() {
	r := recover()
	if r == nil {
		return
	}
	var err error
	switch v := r.(type) {
	case error:
		err = v
	case string:
		err = errors.New(v)
	default:
		err = fmt.Errorf("%v", v)
	}
	if client := GetGlobalClient(); client != nil {
		client.ReportError(err)
	}
	panic(r)
}
