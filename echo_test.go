package middlemonitor

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

// ── responseWriterWrapper ─────────────────────────────────────────────────────

func TestResponseWriterWrapper_WriteHeader_Idempotent(t *testing.T) {
	rec := httptest.NewRecorder()
	status := 200
	w := &responseWriterWrapper{
		ResponseWriter: rec,
		statusCode:     &status,
	}
	w.WriteHeader(201)
	w.WriteHeader(202) // second call should be ignored
	if status != 201 {
		t.Errorf("want 201, got %d", status)
	}
}

func TestResponseWriterWrapper_Write_ImplicitHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	status := 200
	w := &responseWriterWrapper{
		ResponseWriter: rec,
		statusCode:     &status,
	}
	// Write without WriteHeader → defaults to 200
	_, _ = w.Write([]byte("hello"))
	if status != 200 {
		t.Errorf("want 200, got %d", status)
	}
}

func TestResponseWriterWrapper_BodyCapture_5xx(t *testing.T) {
	rec := httptest.NewRecorder()
	status := 200
	bodyBuf := bytes.NewBuffer(nil)
	w := &responseWriterWrapper{
		ResponseWriter: rec,
		statusCode:     &status,
		bodyCapture:    bodyBuf,
		maxCapture:     100,
	}
	w.WriteHeader(500)
	_, _ = w.Write([]byte(`{"error":"db down"}`))
	if bodyBuf.Len() == 0 {
		t.Error("expected body to be captured for 5xx")
	}
}

func TestResponseWriterWrapper_BodyCapture_MaxLimit(t *testing.T) {
	rec := httptest.NewRecorder()
	status := 200
	bodyBuf := bytes.NewBuffer(nil)
	w := &responseWriterWrapper{
		ResponseWriter: rec,
		statusCode:     &status,
		bodyCapture:    bodyBuf,
		maxCapture:     5,
	}
	w.WriteHeader(500)
	_, _ = w.Write([]byte("0123456789"))
	if bodyBuf.Len() != 5 {
		t.Errorf("body capture should be capped at 5, got %d", bodyBuf.Len())
	}
}

func TestResponseWriterWrapper_BodyCapture_NilBuf(t *testing.T) {
	rec := httptest.NewRecorder()
	status := 200
	w := &responseWriterWrapper{
		ResponseWriter: rec,
		statusCode:     &status,
		bodyCapture:    nil, // no capture buffer
	}
	w.WriteHeader(500)
	_, _ = w.Write([]byte("some body"))
	// Should not panic
}

// ── panicRecoveryMiddleware is the outermost Echo middleware in tests:
// it absorbs any re-panic from EchoMiddleware so the test server doesn't crash.
func panicRecoveryMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) (retErr error) {
			defer func() {
				if r := recover(); r != nil {
					c.Response().Writer.WriteHeader(http.StatusInternalServerError)
				}
			}()
			return next(c)
		}
	}
}

// ── EchoMiddleware ────────────────────────────────────────────────────────────

func TestEchoMiddleware_NoClient(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	e := echo.New()
	e.Use(EchoMiddleware())
	e.GET("/ping", func(c echo.Context) error {
		return c.String(http.StatusOK, "pong")
	})

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
}

func TestEchoMiddleware_2xx(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	defer otlpSrv.Close()

	Init(NewConfig(otlpSrv.URL, "svc", "tok"))

	e := echo.New()
	e.Use(EchoMiddleware())
	e.GET("/api/data", func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
}

func TestEchoMiddleware_4xx(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	defer otlpSrv.Close()

	Init(NewConfig(otlpSrv.URL, "svc", "tok"))

	e := echo.New()
	e.Use(EchoMiddleware())
	e.GET("/notfound", func(c echo.Context) error {
		return c.String(http.StatusNotFound, "not found")
	})

	req := httptest.NewRequest(http.MethodGet, "/notfound", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
}

func TestEchoMiddleware_5xx_JSONBody(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	e := echo.New()
	e.Use(EchoMiddleware())
	e.GET("/fail", func(c echo.Context) error {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{"error": "db down"})
	})

	req := httptest.NewRequest(http.MethodGet, "/fail", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rec.Code)
	}
	time.Sleep(150 * time.Millisecond)
}

func TestEchoMiddleware_HandlerReturnsError_Unwritten(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	e := echo.New()
	e.Use(EchoMiddleware())
	e.GET("/err", func(c echo.Context) error {
		// Return error without writing response → middleware writes 500
		return errors.New("handler error")
	})

	req := httptest.NewRequest(http.MethodGet, "/err", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rec.Code)
	}
	time.Sleep(150 * time.Millisecond)
}

func TestEchoMiddleware_5xx_WithKeyExceptionMessage(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	e := echo.New()
	e.Use(EchoMiddleware())
	e.GET("/crash", func(c echo.Context) error {
		c.Set(KeyExceptionMessage, "real error message")
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{})
	})

	req := httptest.NewRequest(http.MethodGet, "/crash", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rec.Code)
	}
	time.Sleep(150 * time.Millisecond)
}

func TestEchoMiddleware_NeverSampleRoute_2xx(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	defer otlpSrv.Close()

	cfg := NewConfig(otlpSrv.URL, "svc", "tok")
	cfg.Sampling.Traces.NeverSampleRoutes = []string{"/health"}
	Init(cfg)

	e := echo.New()
	e.Use(EchoMiddleware())
	e.GET("/health", func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
}

func TestEchoMiddleware_NeverSampleRoute_With5xx(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	cfg := NewConfig(backendSrv.URL, "svc", "tok")
	cfg.Sampling.Traces.NeverSampleRoutes = []string{"/health"}
	cfg.Sampling.Traces.AlwaysSampleErrors = true
	Init(cfg)

	e := echo.New()
	e.Use(EchoMiddleware())
	e.GET("/health", func(c echo.Context) error {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{"error": "db"})
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rec.Code)
	}
	time.Sleep(150 * time.Millisecond)
}

func TestEchoMiddleware_Panic_ErrorType(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	e := echo.New()
	// RecoveryWrapper outermost so it catches the re-panic from EchoMiddleware
	e.Use(panicRecoveryMiddleware())
	e.Use(EchoMiddleware())
	e.GET("/panic-err", func(c echo.Context) error {
		panic(errors.New("panic error"))
	})

	req := httptest.NewRequest(http.MethodGet, "/panic-err", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

func TestEchoMiddleware_Panic_StringType(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	e := echo.New()
	e.Use(panicRecoveryMiddleware())
	e.Use(EchoMiddleware())
	e.GET("/panic-str", func(c echo.Context) error {
		panic("string panic")
	})

	req := httptest.NewRequest(http.MethodGet, "/panic-str", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

func TestEchoMiddleware_Panic_OtherType(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	e := echo.New()
	e.Use(panicRecoveryMiddleware())
	e.Use(EchoMiddleware())
	e.GET("/panic-other", func(c echo.Context) error {
		panic(42)
	})

	req := httptest.NewRequest(http.MethodGet, "/panic-other", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

func TestEchoMiddleware_Panic_NoSpan(t *testing.T) {
	// Panic on a route where sampling=0 (no span created) → middleware creates an error span
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	cfg := NewConfig(backendSrv.URL, "svc", "tok")
	cfg.Sampling.Traces.Percentage = 0
	cfg.Sampling.Traces.AlwaysSampleErrors = false
	Init(cfg)

	e := echo.New()
	e.Use(panicRecoveryMiddleware())
	e.Use(EchoMiddleware())
	e.GET("/no-span", func(c echo.Context) error {
		panic(errors.New("panic no span"))
	})

	req := httptest.NewRequest(http.MethodGet, "/no-span", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

func TestEchoMiddleware_WithRequestBody(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	e := echo.New()
	e.Use(EchoMiddleware())
	e.POST("/api", func(c echo.Context) error {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{"error": "fail"})
	})

	body := strings.NewReader(`{"user":"test"}`)
	req := httptest.NewRequest(http.MethodPost, "/api", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

func TestEchoMiddleware_WithLargeRequestBody(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	e := echo.New()
	e.Use(EchoMiddleware())
	e.POST("/api", func(c echo.Context) error {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{"error": "fail"})
	})

	// Body > 10000 bytes → middleware truncates to 10000
	largeBody := strings.NewReader(strings.Repeat("x", 11000))
	req := httptest.NewRequest(http.MethodPost, "/api", largeBody)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

func TestEchoMiddleware_TraceContextPropagation(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	defer otlpSrv.Close()

	Init(NewConfig(otlpSrv.URL, "svc", "tok"))

	e := echo.New()
	e.Use(EchoMiddleware())
	e.GET("/trace", func(c echo.Context) error {
		return c.String(http.StatusOK, "traced")
	})

	req := httptest.NewRequest(http.MethodGet, "/trace", nil)
	// Add W3C trace context header
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
}

// TestEchoMiddleware_NilClient_BlockedAutoInit covers echo.go:75 — the nil-client
// early return — with no token set, so GetGlobalClient() never auto-inits.
func TestEchoMiddleware_NilClient_BlockedAutoInit(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	t.Setenv("MIDDLE_MONITOR_TOKEN", "")

	e := echo.New()
	e.Use(EchoMiddleware())
	e.GET("/ping", func(c echo.Context) error {
		return c.String(http.StatusOK, "pong")
	})

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
}

// TestEchoMiddleware_EmptyRoute covers echo.go:90 — the fallback to URL path
// when c.Path() returns "" (request to an unregistered route).
func TestEchoMiddleware_EmptyRoute(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	defer otlpSrv.Close()
	Init(NewConfig(otlpSrv.URL, "svc", "tok"))

	e := echo.New()
	e.Use(EchoMiddleware())
	// No routes registered → any request has c.Path() == ""

	req := httptest.NewRequest(http.MethodGet, "/unknown-path", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	// Echo returns 404 for unregistered routes; middleware still runs with route=""
}

// TestEchoMiddleware_5xx_HandlerWritesAndReturnsError covers echo.go:218 — the
// "if err != nil" branch inside the span update block. The handler writes a 500
// response AND returns a non-nil error, so err is non-nil after the auto-hide check
// (which only fires when !wrapper.written).
func TestEchoMiddleware_5xx_HandlerWritesAndReturnsError(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	e := echo.New()
	e.Use(EchoMiddleware())
	e.GET("/err-after-write", func(c echo.Context) error {
		// Write 500 first (wrapper.written = true), then return an error
		c.Response().WriteHeader(http.StatusInternalServerError)
		return errors.New("error after write")
	})

	req := httptest.NewRequest(http.MethodGet, "/err-after-write", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

// TestEchoMiddleware_NeverSample_5xx_HandlerWritesAndReturnsError covers echo.go:244
// — the "if err != nil" branch inside the error-span block for unsampled routes.
func TestEchoMiddleware_NeverSample_5xx_HandlerWritesAndReturnsError(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	cfg := NewConfig(backendSrv.URL, "svc", "tok")
	cfg.Sampling.Traces.NeverSampleRoutes = []string{"/internal"}
	cfg.Sampling.Traces.AlwaysSampleErrors = true
	Init(cfg)

	e := echo.New()
	e.Use(EchoMiddleware())
	e.GET("/internal", func(c echo.Context) error {
		// Write 500 so wrapper.written=true, then return error so err != nil
		c.Response().WriteHeader(http.StatusInternalServerError)
		return errors.New("internal error after write")
	})

	req := httptest.NewRequest(http.MethodGet, "/internal", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

// TestReportExceptionFromEcho_WithContext covers echo.go:433 — the non-nil context path.
func TestReportExceptionFromEcho_WithContext(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	backendSrv := startBackendErrorsServer(t)
	defer backendSrv.Close()
	globalConfig = &Config{Endpoint: backendSrv.URL, Service: "s"}

	e := echo.New()
	var capturedContext echo.Context
	e.GET("/test", func(c echo.Context) error {
		capturedContext = c
		return nil
	})
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if capturedContext != nil {
		ReportExceptionFromEcho(capturedContext, "test exception")
		time.Sleep(100 * time.Millisecond)
	}
}

// TestSubmitApplicationError_BadEndpoint covers echo.go:476-478 — early return when
// http.NewRequestWithContext fails due to an invalid endpoint URL.
func TestSubmitApplicationError_BadEndpoint(t *testing.T) {
	cfg := &Config{Endpoint: "http://[invalid", Service: "svc"}
	// Should not panic
	submitApplicationError(cfg, "TypeError", "msg", "file.go", 10, 500, "GET", "/path", nil)
}

// ── Test helpers ──────────────────────────────────────────────────────────────

// startOTLPServer creates a test server that accepts any OTLP request.
func startOTLPServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

// startBackendErrorsServer creates a test server that accepts /api/v1/errors.
func startBackendErrorsServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}
