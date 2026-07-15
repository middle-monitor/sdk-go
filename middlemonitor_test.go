package middlemonitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ── NewClient ─────────────────────────────────────────────────────────────────

func TestNewClient_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "svc")
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}

// ── SetToken ─────────────────────────────────────────────────────────────────

func TestSetToken_WithConfig(t *testing.T) {
	cfg := NewConfig("http://localhost:8080", "svc", "old")
	c := &Client{config: cfg}
	c.SetToken("new-token")
	if cfg.Token != "new-token" {
		t.Errorf("want new-token, got %q", cfg.Token)
	}
}

func TestSetToken_NilConfig(t *testing.T) {
	c := &Client{config: nil}
	// Should not panic
	c.SetToken("token")
}

// ── ReportError ───────────────────────────────────────────────────────────────

func TestReportError_NilError(t *testing.T) {
	c := &Client{}
	err := c.ReportError(nil)
	if err != nil {
		t.Errorf("nil error should return nil, got %v", err)
	}
}

func TestReportError_NilClient(t *testing.T) {
	var c *Client
	err := c.ReportErrorWithDetails(errors.New("oops"), "", 0)
	if err != ErrNotInitialized {
		t.Errorf("want ErrNotInitialized, got %v", err)
	}
}

func TestReportError_NoTracer(t *testing.T) {
	c := &Client{config: NewConfig("localhost", "svc", "")}
	err := c.ReportErrorWithDetails(errors.New("oops"), "", 0)
	if err != ErrNotInitialized {
		t.Errorf("want ErrNotInitialized, got %v", err)
	}
}

func TestReportErrorWithDetails_AutoFileDetection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resetGlobalState()
	defer resetGlobalState()
	if err := Init(NewConfig(srv.URL, "svc", "tok")); err != nil {
		t.Fatalf("init: %v", err)
	}
	c := GetGlobalClient()
	if c == nil {
		t.Fatal("global client is nil")
	}
	// file="" → auto-detect from runtime.Caller
	err := c.ReportErrorWithDetails(errors.New("test error"), "", 0)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestReportErrorWithDetails_WithFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NewConfig(srv.URL, "svc", "tok")
	c, err := NewClientWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewClientWithConfig: %v", err)
	}
	if err2 := c.ReportErrorWithDetails(errors.New("test"), "main.go", 42); err2 != nil {
		t.Errorf("unexpected error: %v", err2)
	}
}

// ── ReportCustomError ─────────────────────────────────────────────────────────

func TestReportCustomError_NoTracer(t *testing.T) {
	c := &Client{config: NewConfig("localhost", "svc", "")}
	err := c.ReportCustomError("DBError", "connection refused", "db.go", 10)
	if err != ErrNotInitialized {
		t.Errorf("want ErrNotInitialized, got %v", err)
	}
}

func TestReportCustomError_NilClient(t *testing.T) {
	var c *Client
	err := c.ReportCustomError("E", "msg", "f.go", 1)
	if err != ErrNotInitialized {
		t.Errorf("want ErrNotInitialized, got %v", err)
	}
}

func TestReportCustomError_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NewConfig(srv.URL, "svc", "tok")
	c, err := NewClientWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewClientWithConfig: %v", err)
	}
	if err2 := c.ReportCustomError("DBError", "conn refused", "db.go", 10); err2 != nil {
		t.Errorf("unexpected error: %v", err2)
	}
}

// ── ReportCustomErrorWithHTTP ─────────────────────────────────────────────────

func TestReportCustomErrorWithHTTP_NoTracer(t *testing.T) {
	c := &Client{config: NewConfig("localhost", "svc", "")}
	err := c.ReportCustomErrorWithHTTP("E", "msg", "f.go", 1, "GET", "/path", "", "")
	if err != ErrNotInitialized {
		t.Errorf("want ErrNotInitialized, got %v", err)
	}
}

func TestReportCustomErrorWithHTTP_NilClient(t *testing.T) {
	var c *Client
	err := c.ReportCustomErrorWithHTTP("E", "msg", "f.go", 1, "POST", "/x", "", "")
	if err != ErrNotInitialized {
		t.Errorf("want ErrNotInitialized, got %v", err)
	}
}

func TestReportCustomErrorWithHTTP_NoHTTPContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NewConfig(srv.URL, "svc", "tok")
	c, err := NewClientWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewClientWithConfig: %v", err)
	}
	// Empty method and URL → those branches not added to attrs
	if err2 := c.ReportCustomErrorWithHTTP("E", "msg", "f.go", 1, "", "", "", ""); err2 != nil {
		t.Errorf("unexpected error: %v", err2)
	}
}

func TestReportCustomErrorWithHTTP_WithHTTPContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NewConfig(srv.URL, "svc", "tok")
	c, err := NewClientWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewClientWithConfig: %v", err)
	}
	if err2 := c.ReportCustomErrorWithHTTP("E", "msg", "f.go", 1, "POST", "/api", "{}", "body"); err2 != nil {
		t.Errorf("unexpected error: %v", err2)
	}
}

// ── CapturePanic ─────────────────────────────────────────────────────────────

func TestCapturePanic_NoPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NewConfig(srv.URL, "svc", "tok")
	c, _ := NewClientWithConfig(cfg)
	defer c.CapturePanic()
	// No panic → no report
}

func TestCapturePanic_ErrorPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NewConfig(srv.URL, "svc", "tok")
	c, _ := NewClientWithConfig(cfg)

	defer func() { recover() }() // suppress re-panic
	defer c.CapturePanic()
	panic(errors.New("test error panic"))
}

func TestCapturePanic_StringPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NewConfig(srv.URL, "svc", "tok")
	c, _ := NewClientWithConfig(cfg)

	defer func() { recover() }()
	defer c.CapturePanic()
	panic("string panic")
}

func TestCapturePanic_OtherTypePanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NewConfig(srv.URL, "svc", "tok")
	c, _ := NewClientWithConfig(cfg)

	defer func() { recover() }()
	defer c.CapturePanic()
	panic(42)
}

// ── Global functions ──────────────────────────────────────────────────────────

func TestGlobalReportError_NoClient(t *testing.T) {
	resetGlobalState()
	// Should not panic
	ReportError(errors.New("test"))
}

func TestGlobalReportErrorWithDetails_NoClient(t *testing.T) {
	resetGlobalState()
	ReportErrorWithDetails(errors.New("test"), "file.go", 10)
}

func TestCapturePanicGlobal_NoPanic(t *testing.T) {
	resetGlobalState()
	// No panic — should be a no-op
	defer CapturePanicGlobal()
}

func TestCapturePanicGlobal_WithPanic_NoClient(t *testing.T) {
	resetGlobalState()
	defer func() { recover() }()
	defer CapturePanicGlobal()
	panic("global panic without client")
}

func TestCapturePanicGlobal_ErrorPanic_WithClient(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	Init(NewConfig(srv.URL, "svc", "tok"))

	defer func() { recover() }()
	defer CapturePanicGlobal()
	panic(fmt.Errorf("error panic with client"))
}

func TestCapturePanicGlobal_StringPanic_WithClient(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	Init(NewConfig(srv.URL, "svc", "tok"))

	defer func() { recover() }()
	defer CapturePanicGlobal()
	panic("string panic with client")
}

func TestCapturePanicGlobal_OtherTypePanic_WithClient(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	Init(NewConfig(srv.URL, "svc", "tok"))

	defer func() { recover() }()
	defer CapturePanicGlobal()
	panic(struct{ msg string }{"other type"})
}

// ── Helper functions (echo.go) ────────────────────────────────────────────────

func TestGetMessageForException_FromErr(t *testing.T) {
	msg := getMessageForException(errors.New("db failed"), 500, nil)
	if msg != "db failed" {
		t.Errorf("want 'db failed', got %q", msg)
	}
}

func TestGetMessageForException_FromBody(t *testing.T) {
	body := bytes.NewBufferString(`{"error":"upstream timeout"}`)
	msg := getMessageForException(nil, 502, body)
	if msg != "upstream timeout" {
		t.Errorf("want 'upstream timeout', got %q", msg)
	}
}

func TestGetMessageForException_BodyNoErrorField(t *testing.T) {
	body := bytes.NewBufferString(`{"message":"something"}`)
	msg := getMessageForException(nil, 502, body)
	if msg != "HTTP 502" {
		t.Errorf("want 'HTTP 502', got %q", msg)
	}
}

func TestGetMessageForException_EmptyBody(t *testing.T) {
	msg := getMessageForException(nil, 503, bytes.NewBufferString(""))
	if msg != "HTTP 503" {
		t.Errorf("want 'HTTP 503', got %q", msg)
	}
}

func TestGetMessageForException_NilBody(t *testing.T) {
	msg := getMessageForException(nil, 503, nil)
	if msg != "HTTP 503" {
		t.Errorf("want 'HTTP 503', got %q", msg)
	}
}

func TestGetMessageForException_InvalidJSON(t *testing.T) {
	body := bytes.NewBufferString("not-json")
	msg := getMessageForException(nil, 500, body)
	if msg != "HTTP 500" {
		t.Errorf("want 'HTTP 500', got %q", msg)
	}
}

func TestIsGenericExceptionMessage(t *testing.T) {
	// Returns true when msg starts with "HTTP " and len(msg) <= 12.
	// Does not validate that it is a real 3-digit status code.
	cases := []struct {
		msg  string
		want bool
	}{
		{"HTTP 500", true},
		{"HTTP 502", true},
		{"HTTP 50", true},   // len=7, starts with "HTTP "
		{"HTTP 5000", true}, // len=9, starts with "HTTP "
		{"HTTP 500: database connection failed", false}, // len > 12
		{"connection refused", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isGenericExceptionMessage(tc.msg)
		if got != tc.want {
			t.Errorf("isGenericExceptionMessage(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

func TestGetHandlerLocation_NoPanel(t *testing.T) {
	// Just verify it runs without panicking
	file, line := getHandlerLocation()
	_ = file
	_ = line
}

func TestGetPanicLocation_NoPanic(t *testing.T) {
	file, line := getPanicLocation()
	_ = file
	_ = line
}

// ── submitApplicationError ────────────────────────────────────────────────────

func TestSubmitApplicationError_NilConfig(t *testing.T) {
	// Should not panic
	submitApplicationError(nil, "http", "msg", "f.go", 10, 500, "GET", "/", nil)
}

func TestSubmitApplicationError_EmptyEndpoint(t *testing.T) {
	cfg := &Config{}
	submitApplicationError(cfg, "http", "msg", "f.go", 10, 500, "GET", "/", nil)
}

func TestSubmitApplicationError_Success(t *testing.T) {
	received := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &Config{
		Endpoint: srv.URL,
		Service:  "svc",
		Token:    "tok",
	}
	submitApplicationError(cfg, "http", "msg", "f.go", 10, 500, "GET", "/api", []byte(`{"key":"val"}`))

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Error("test server did not receive the request")
	}
}

func TestSubmitApplicationError_LargeBody(t *testing.T) {
	received := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &Config{Endpoint: srv.URL, Service: "svc"}
	largeBody := make([]byte, 3000)
	submitApplicationError(cfg, "http", "msg", "f.go", 10, 500, "", "", largeBody)

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Error("test server did not receive the request")
	}
}

func TestSubmitApplicationError_ServerReturns4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	cfg := &Config{Endpoint: srv.URL, Service: "svc"}
	// Should log warning but not panic
	submitApplicationError(cfg, "http", "msg", "f.go", 10, 500, "GET", "/api", nil)
	time.Sleep(100 * time.Millisecond)
}

func TestSubmitApplicationError_ServerUnreachable(t *testing.T) {
	cfg := &Config{Endpoint: "http://localhost:19996", Service: "svc"}
	// Should log error but not panic
	submitApplicationError(cfg, "http", "msg", "f.go", 10, 500, "GET", "/api", nil)
	time.Sleep(100 * time.Millisecond)
}

// ── reportException ──────────────────────────────────────────────────────────

func TestReportException_LowStatusCode(t *testing.T) {
	resetGlobalState()
	// status < 500 → return immediately
	reportException("msg", 400, "GET", "/api")
}

func TestReportException_NoGlobalConfig(t *testing.T) {
	resetGlobalState()
	// No config → return immediately
	reportException("msg", 500, "GET", "/api")
}

func TestReportException_EmptyEndpoint(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	globalConfig = &Config{Endpoint: ""}
	reportException("msg", 500, "GET", "/api")
}

func TestReportException_WithServer(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	globalConfig = &Config{
		Endpoint: srv.URL,
		Service:  "svc",
	}
	reportException("something failed", 0, "", "")
	time.Sleep(100 * time.Millisecond)
}

func TestReportException_FileEmptyFallback(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	globalConfig = &Config{Endpoint: srv.URL, Service: "s"}
	reportException("msg", 500, "POST", "/path")
	time.Sleep(100 * time.Millisecond)
}

// ── ReportException / ReportExceptionFromEcho / ReportExceptionFromGin ────────

func TestReportExceptionPublic(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	globalConfig = &Config{Endpoint: "http://localhost:19995"}
	// Should not panic (fire-and-forget, server not running)
	ReportException("something went wrong")
	time.Sleep(50 * time.Millisecond)
}

func TestReportExceptionFromEcho_NilContext(t *testing.T) {
	// Should not panic
	ReportExceptionFromEcho(nil, "msg")
}

func TestReportExceptionFromGin_NilContext(t *testing.T) {
	// Should not panic
	ReportExceptionFromGin(nil, "msg")
}

// ── ReportExceptionWithContext ────────────────────────────────────────────────

func TestReportExceptionWithContext_NilErr(t *testing.T) {
	// Should be a no-op
	ReportExceptionWithContext(context.Background(), nil)
}

func TestReportExceptionWithContext_NilCtx(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	globalConfig = &Config{Endpoint: "http://localhost:19994"}
	ReportExceptionWithContext(nil, errors.New("err"))
	time.Sleep(50 * time.Millisecond)
}

func TestReportExceptionWithContext_WithRequestInfo(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	globalConfig = &Config{Endpoint: srv.URL, Service: "s"}
	ctx := context.WithValue(context.Background(), requestContextKey{}, &requestInfo{Method: "GET", URL: "/api"})
	ReportExceptionWithContext(ctx, errors.New("req error"))
	time.Sleep(100 * time.Millisecond)
}

// setter implements the interface that ReportExceptionWithContext uses for framework contexts.
type testSetter struct{ vals map[string]interface{} }

func (s *testSetter) Set(key string, val interface{}) { s.vals[key] = val }

func TestReportExceptionWithContext_WithFrameworkContext(t *testing.T) {
	setter := &testSetter{vals: map[string]interface{}{}}
	ctx := context.WithValue(context.Background(), frameworkContextKey{}, setter)
	ReportExceptionWithContext(ctx, errors.New("framework err"))
	if setter.vals[KeyExceptionMessage] != "framework err" {
		t.Errorf("want 'framework err' in setter, got %v", setter.vals[KeyExceptionMessage])
	}
}
