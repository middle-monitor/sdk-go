package middlemonitor

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// ── ginResponseWriter ─────────────────────────────────────────────────────────

func TestGinResponseWriter_WriteHeader_Idempotent(t *testing.T) {
	// Build a real gin ResponseWriter around the recorder
	e := gin.New()
	var captured *int
	e.GET("/", func(c *gin.Context) {
		status := 200
		bodyBuf := bytes.NewBuffer(nil)
		w := &ginResponseWriter{
			ResponseWriter: c.Writer,
			statusCode:     &status,
			bodyCapture:    bodyBuf,
			maxCapture:     100,
		}
		w.WriteHeader(201)
		w.WriteHeader(202) // second call should be ignored
		captured = &status
		c.Status(201)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	resp := httptest.NewRecorder()
	e.ServeHTTP(resp, req)

	if captured != nil && *captured != 201 {
		t.Errorf("want 201, got %d", *captured)
	}
}

func TestGinResponseWriter_BodyCapture_5xx(t *testing.T) {
	e := gin.New()
	var bodyBuf *bytes.Buffer
	e.GET("/", func(c *gin.Context) {
		bodyBuf = bytes.NewBuffer(nil)
		w := &ginResponseWriter{
			ResponseWriter: c.Writer,
			statusCode:     func() *int { s := 200; return &s }(),
			bodyCapture:    bodyBuf,
			maxCapture:     100,
		}
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"fail"}`))
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	resp := httptest.NewRecorder()
	e.ServeHTTP(resp, req)

	if bodyBuf != nil && bodyBuf.Len() == 0 {
		t.Error("expected body to be captured for 5xx")
	}
}

// ── GinMiddleware ──────────────────────────────────────────────────────────────

func TestGinMiddleware_NoClient(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	r := gin.New()
	r.Use(GinMiddleware())
	r.GET("/ping", func(c *gin.Context) {
		c.String(http.StatusOK, "pong")
	})

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
}

func TestGinMiddleware_2xx(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	defer otlpSrv.Close()

	Init(NewConfig(otlpSrv.URL, "svc", "tok"))

	r := gin.New()
	r.Use(GinMiddleware())
	r.GET("/api/data", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
}

func TestGinMiddleware_4xx(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	defer otlpSrv.Close()

	Init(NewConfig(otlpSrv.URL, "svc", "tok"))

	r := gin.New()
	r.Use(GinMiddleware())
	r.GET("/notfound", func(c *gin.Context) {
		c.String(http.StatusNotFound, "not found")
	})

	req := httptest.NewRequest(http.MethodGet, "/notfound", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
}

func TestGinMiddleware_5xx_JSONBody(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	r := gin.New()
	r.Use(GinMiddleware())
	r.GET("/fail", func(c *gin.Context) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db down"})
	})

	req := httptest.NewRequest(http.MethodGet, "/fail", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rec.Code)
	}
	time.Sleep(150 * time.Millisecond)
}

func TestGinMiddleware_5xx_WithGinError(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	r := gin.New()
	r.Use(GinMiddleware())
	r.GET("/gin-err", func(c *gin.Context) {
		_ = c.Error(errors.New("gin handler error"))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
	})

	req := httptest.NewRequest(http.MethodGet, "/gin-err", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rec.Code)
	}
	time.Sleep(150 * time.Millisecond)
}

func TestGinMiddleware_5xx_WithKeyExceptionMessage(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	r := gin.New()
	r.Use(GinMiddleware())
	r.GET("/crash", func(c *gin.Context) {
		c.Set(KeyExceptionMessage, "real error message")
		c.JSON(http.StatusInternalServerError, gin.H{})
	})

	req := httptest.NewRequest(http.MethodGet, "/crash", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rec.Code)
	}
	time.Sleep(150 * time.Millisecond)
}

func TestGinMiddleware_NeverSampleRoute_2xx(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	defer otlpSrv.Close()

	cfg := NewConfig(otlpSrv.URL, "svc", "tok")
	cfg.Sampling.Traces.NeverSampleRoutes = []string{"/health"}
	Init(cfg)

	r := gin.New()
	r.Use(GinMiddleware())
	r.GET("/health", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
}

func TestGinMiddleware_Panic_ErrorType(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	r := gin.New()
	// Gin's built-in recovery is the outermost (catches re-panic from GinMiddleware)
	r.Use(gin.CustomRecovery(func(c *gin.Context, recovered interface{}) {
		c.AbortWithStatus(http.StatusInternalServerError)
	}))
	r.Use(GinMiddleware())
	r.GET("/panic-err", func(c *gin.Context) {
		panic(errors.New("panic error"))
	})

	req := httptest.NewRequest(http.MethodGet, "/panic-err", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

func TestGinMiddleware_Panic_NoSpan(t *testing.T) {
	// Panic on a route where sampling=0 (no span created) → middleware creates an error span
	resetGlobalState()
	defer resetGlobalState()

	backendSrv := startBackendErrorsServer(t)
	defer backendSrv.Close()

	cfg := NewConfig(backendSrv.URL, "svc", "tok")
	cfg.Sampling.Traces.Percentage = 0
	cfg.Sampling.Traces.AlwaysSampleErrors = false
	Init(cfg)

	r := gin.New()
	r.Use(gin.CustomRecovery(func(c *gin.Context, recovered interface{}) {
		c.AbortWithStatus(http.StatusInternalServerError)
	}))
	r.Use(GinMiddleware())
	r.GET("/no-span", func(c *gin.Context) {
		panic(errors.New("panic no span"))
	})

	req := httptest.NewRequest(http.MethodGet, "/no-span", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

func TestGinMiddleware_NeverSampleRoute_With5xx(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	backendSrv := startBackendErrorsServer(t)
	defer backendSrv.Close()

	cfg := NewConfig(backendSrv.URL, "svc", "tok")
	cfg.Sampling.Traces.NeverSampleRoutes = []string{"/health"}
	cfg.Sampling.Traces.AlwaysSampleErrors = true
	Init(cfg)

	r := gin.New()
	r.Use(GinMiddleware())
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusInternalServerError, map[string]interface{}{"error": "db"})
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rec.Code)
	}
	time.Sleep(150 * time.Millisecond)
}

func TestGinMiddleware_Panic_StringType(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	r := gin.New()
	r.Use(gin.CustomRecovery(func(c *gin.Context, recovered interface{}) {
		c.AbortWithStatus(http.StatusInternalServerError)
	}))
	r.Use(GinMiddleware())
	r.GET("/panic-str", func(c *gin.Context) {
		panic("string panic")
	})

	req := httptest.NewRequest(http.MethodGet, "/panic-str", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

func TestGinMiddleware_Panic_OtherType(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	r := gin.New()
	r.Use(gin.CustomRecovery(func(c *gin.Context, recovered interface{}) {
		c.AbortWithStatus(http.StatusInternalServerError)
	}))
	r.Use(GinMiddleware())
	r.GET("/panic-other", func(c *gin.Context) {
		panic(struct{ val int }{42})
	})

	req := httptest.NewRequest(http.MethodGet, "/panic-other", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

func TestGinMiddleware_WithRequestBody(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	r := gin.New()
	r.Use(GinMiddleware())
	r.POST("/api", func(c *gin.Context) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "fail"})
	})

	body := strings.NewReader(`{"user":"test"}`)
	req := httptest.NewRequest(http.MethodPost, "/api", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

func TestGinMiddleware_WithLargeRequestBody(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	r := gin.New()
	r.Use(GinMiddleware())
	r.POST("/api", func(c *gin.Context) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "fail"})
	})

	largeBody := strings.NewReader(strings.Repeat("x", 11000))
	req := httptest.NewRequest(http.MethodPost, "/api", largeBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

func TestGinMiddleware_TraceContextPropagation(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	defer otlpSrv.Close()

	Init(NewConfig(otlpSrv.URL, "svc", "tok"))

	r := gin.New()
	r.Use(GinMiddleware())
	r.GET("/trace", func(c *gin.Context) {
		c.String(http.StatusOK, "traced")
	})

	req := httptest.NewRequest(http.MethodGet, "/trace", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
}

// TestGinMiddleware_NilClient_BlockedAutoInit covers gin.go:62 — nil client path.
func TestGinMiddleware_NilClient_BlockedAutoInit(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	initOnce.Do(func() {})

	r := gin.New()
	r.Use(GinMiddleware())
	r.GET("/ping", func(c *gin.Context) {
		c.String(http.StatusOK, "pong")
	})

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
}

// TestGinMiddleware_EmptyFullPath covers gin.go:78 — fallback to URL path when
// c.FullPath() returns "" (request to an unregistered route in Gin).
func TestGinMiddleware_EmptyFullPath(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	defer otlpSrv.Close()
	Init(NewConfig(otlpSrv.URL, "svc", "tok"))

	r := gin.New()
	r.Use(GinMiddleware())
	// No routes registered → FullPath() returns "" for any request

	req := httptest.NewRequest(http.MethodGet, "/unknown", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	// Gin returns 404 for unregistered routes; middleware still runs
}

// TestGinMiddleware_5xx_HandlerWritesAndReturnsError covers gin.go spans' err!=nil branch
// when the handler writes a 5xx response AND adds a Gin error.
func TestGinMiddleware_5xx_HandlerWritesAndReturnsError(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	backendSrv := startBackendErrorsServer(t)
	defer otlpSrv.Close()
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	r := gin.New()
	r.Use(GinMiddleware())
	r.GET("/err", func(c *gin.Context) {
		_ = c.Error(errors.New("gin handler error"))
		c.Status(http.StatusInternalServerError)
	})

	req := httptest.NewRequest(http.MethodGet, "/err", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

// TestGinResponseWriter_Write_ImplicitHeader covers gin.go:39 — the implicit
// WriteHeader(200) inside Write when !w.written.
func TestGinResponseWriter_Write_ImplicitHeader(t *testing.T) {
	r := gin.New()
	var captured int
	r.GET("/", func(c *gin.Context) {
		status := 200
		w := &ginResponseWriter{
			ResponseWriter: c.Writer,
			statusCode:     &status,
		}
		// Write without calling WriteHeader first → triggers implicit WriteHeader(200)
		_, _ = w.Write([]byte("hello"))
		captured = status
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if captured != 200 {
		t.Errorf("want implicit status 200, got %d", captured)
	}
}

// TestGinResponseWriter_Write_BodyCaptureTruncation covers gin.go:44 — the
// truncation branch when len(b) > remain.
func TestGinResponseWriter_Write_BodyCaptureTruncation(t *testing.T) {
	r := gin.New()
	var bodyBuf *bytes.Buffer
	r.GET("/", func(c *gin.Context) {
		status := 500
		bodyBuf = bytes.NewBuffer(nil)
		w := &ginResponseWriter{
			ResponseWriter: c.Writer,
			statusCode:     &status,
			written:        true, // already written 500
			bodyCapture:    bodyBuf,
			maxCapture:     5,
		}
		// len("0123456789") = 10 > remain=5 → truncation branch
		_, _ = w.Write([]byte("0123456789"))
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if bodyBuf != nil && bodyBuf.Len() != 5 {
		t.Errorf("want 5 bytes captured, got %d", bodyBuf.Len())
	}
}

// TestReportExceptionFromGin_WithContext covers gin.go:225 — non-nil context path.
func TestReportExceptionFromGin_WithContext(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	backendSrv := startBackendErrorsServer(t)
	defer backendSrv.Close()
	globalConfig = &Config{Endpoint: backendSrv.URL, Service: "s"}

	r := gin.New()
	var capturedCtx *gin.Context
	r.GET("/test", func(c *gin.Context) {
		capturedCtx = c
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if capturedCtx != nil {
		ReportExceptionFromGin(capturedCtx, "test exception")
		time.Sleep(100 * time.Millisecond)
	}
}
