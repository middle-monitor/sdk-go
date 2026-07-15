package middlemonitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ── apiBase ──────────────────────────────────────────────────────────────────

func TestAPIBase_Insecure(t *testing.T) {
	cfg := &Config{Endpoint: "host:8080", Insecure: true}
	got := apiBase(cfg)
	if got != "http://host:8080" {
		t.Errorf("want http://host:8080, got %q", got)
	}
}

func TestAPIBase_Secure(t *testing.T) {
	cfg := &Config{Endpoint: "host:8080", Insecure: false}
	got := apiBase(cfg)
	if got != "https://host:8080" {
		t.Errorf("want https://host:8080, got %q", got)
	}
}

func TestAPIBase_AlreadyHasScheme(t *testing.T) {
	cfg := &Config{Endpoint: "http://host:8080"}
	got := apiBase(cfg)
	if got != "http://host:8080" {
		t.Errorf("want http://host:8080, got %q", got)
	}
}

func TestAPIBase_TrailingSlash(t *testing.T) {
	cfg := &Config{Endpoint: "http://host:8080/", Insecure: true}
	got := apiBase(cfg)
	if got != "http://host:8080" {
		t.Errorf("want no trailing slash, got %q", got)
	}
}

// ── uploadProfile ─────────────────────────────────────────────────────────────

func TestUploadProfile_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &Config{
		Endpoint: srv.URL,
		Insecure: true,
		Token:    "tok",
		Service:  "svc",
	}
	secs := 5
	err := uploadProfile(cfg, "cpu", &secs, nil, []byte("fake-pprof-data"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUploadProfile_WithMemoryMB(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &Config{
		Endpoint: srv.URL,
		Insecure: true,
		Token:    "tok",
		Service:  "svc",
	}
	mb := 256.5
	err := uploadProfile(cfg, "heap", nil, &mb, []byte("heap-data"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUploadProfile_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad request"))
	}))
	defer srv.Close()

	cfg := &Config{Endpoint: srv.URL, Insecure: true, Token: "tok", Service: "s"}
	err := uploadProfile(cfg, "cpu", nil, nil, []byte("data"))
	if err == nil {
		t.Error("expected error for 4xx response")
	}
}

func TestUploadProfile_BadURL(t *testing.T) {
	cfg := &Config{Endpoint: "http://[invalid", Insecure: true, Token: "tok"}
	err := uploadProfile(cfg, "cpu", nil, nil, []byte("data"))
	if err == nil {
		t.Error("expected error for invalid URL")
	}
}

// ── CaptureCPUProfile ─────────────────────────────────────────────────────────

func TestCaptureCPUProfile_MissingConfig(t *testing.T) {
	c := &Client{config: &Config{}}
	err := c.CaptureCPUProfile(context.Background(), time.Second)
	if err != ErrConfigRequired {
		t.Errorf("want ErrConfigRequired, got %v", err)
	}
}

func TestCaptureCPUProfile_PprofServerNotFound(t *testing.T) {
	// No pprof server running on this port
	c := &Client{config: &Config{
		Endpoint: "localhost:8080",
		Token:    "tok",
		Insecure: true,
		PprofURL: "http://localhost:19998",
		Service:  "svc",
	}}
	err := c.CaptureCPUProfile(context.Background(), time.Second)
	if err == nil {
		t.Error("expected error when pprof server not reachable")
	}
}

func TestCaptureCPUProfile_PprofReturnsError(t *testing.T) {
	pprofSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("pprof error"))
	}))
	defer pprofSrv.Close()

	c := &Client{config: &Config{
		Endpoint: "localhost:8080",
		Token:    "tok",
		Insecure: true,
		PprofURL: pprofSrv.URL,
		Service:  "svc",
	}}
	err := c.CaptureCPUProfile(context.Background(), time.Second)
	if err == nil {
		t.Error("expected error for pprof server error")
	}
}

func TestCaptureCPUProfile_EmptyProfile(t *testing.T) {
	pprofSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Return empty body
	}))
	defer pprofSrv.Close()

	c := &Client{config: &Config{
		Endpoint: "localhost:8080",
		Token:    "tok",
		Insecure: true,
		PprofURL: pprofSrv.URL,
		Service:  "svc",
	}}
	err := c.CaptureCPUProfile(context.Background(), time.Second)
	if err == nil {
		t.Error("expected error for empty profile")
	}
}

func TestCaptureCPUProfile_DurationClamping(t *testing.T) {
	// duration < 1s should clamp to 1s; duration > 120s to 120s
	// We verify via a test server that receives the query param
	querySecs := make(chan string, 1)
	pprofSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		querySecs <- r.URL.Query().Get("seconds")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("pprof-data"))
	}))
	uploadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer pprofSrv.Close()
	defer uploadSrv.Close()

	c := &Client{config: &Config{
		Endpoint: uploadSrv.URL,
		Token:    "tok",
		Insecure: true,
		PprofURL: pprofSrv.URL,
		Service:  "svc",
	}}

	// Under 1s → clamped to 1
	_ = c.CaptureCPUProfile(context.Background(), 0)
	select {
	case secs := <-querySecs:
		if secs != "1" {
			t.Errorf("want seconds=1, got %q", secs)
		}
	case <-time.After(2 * time.Second):
		t.Error("timeout waiting for pprof request")
	}

	// Over 120s → clamped to 120
	_ = c.CaptureCPUProfile(context.Background(), 200*time.Second)
	select {
	case secs := <-querySecs:
		if secs != "120" {
			t.Errorf("want seconds=120, got %q", secs)
		}
	case <-time.After(2 * time.Second):
		t.Error("timeout waiting for pprof request")
	}
}

func TestCaptureCPUProfile_DefaultPprofURL(t *testing.T) {
	// When PprofURL is empty, it should default to http://localhost:6060 (which won't be running)
	c := &Client{config: &Config{
		Endpoint: "localhost:8080",
		Token:    "tok",
		Insecure: true,
		PprofURL: "",
		Service:  "svc",
	}}
	err := c.CaptureCPUProfile(context.Background(), time.Second)
	if err == nil {
		t.Error("expected error when no pprof server available")
	}
}

// ── CaptureHeapProfile ────────────────────────────────────────────────────────

func TestCaptureHeapProfile_MissingConfig(t *testing.T) {
	c := &Client{config: &Config{}}
	err := c.CaptureHeapProfile(context.Background())
	if err != ErrConfigRequired {
		t.Errorf("want ErrConfigRequired, got %v", err)
	}
}

func TestCaptureHeapProfile_PprofServerNotFound(t *testing.T) {
	c := &Client{config: &Config{
		Endpoint: "localhost:8080",
		Token:    "tok",
		Insecure: true,
		PprofURL: "http://localhost:19997",
		Service:  "svc",
	}}
	err := c.CaptureHeapProfile(context.Background())
	if err == nil {
		t.Error("expected error when pprof server not reachable")
	}
}

func TestCaptureHeapProfile_PprofReturnsError(t *testing.T) {
	pprofSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("heap error"))
	}))
	defer pprofSrv.Close()

	c := &Client{config: &Config{
		Endpoint: "localhost:8080",
		Token:    "tok",
		Insecure: true,
		PprofURL: pprofSrv.URL,
		Service:  "svc",
	}}
	err := c.CaptureHeapProfile(context.Background())
	if err == nil {
		t.Error("expected error for pprof server error")
	}
}

func TestCaptureHeapProfile_EmptyProfile(t *testing.T) {
	pprofSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer pprofSrv.Close()

	c := &Client{config: &Config{
		Endpoint: "localhost:8080",
		Token:    "tok",
		Insecure: true,
		PprofURL: pprofSrv.URL,
		Service:  "svc",
	}}
	err := c.CaptureHeapProfile(context.Background())
	if err == nil {
		t.Error("expected error for empty heap profile")
	}
}

func TestCaptureHeapProfile_Success(t *testing.T) {
	pprofSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("fake-heap-data"))
	}))
	uploadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer pprofSrv.Close()
	defer uploadSrv.Close()

	c := &Client{config: &Config{
		Endpoint: uploadSrv.URL,
		Token:    "tok",
		Insecure: true,
		PprofURL: pprofSrv.URL,
		Service:  "svc",
	}}
	err := c.CaptureHeapProfile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCaptureHeapProfile_DefaultPprofURL(t *testing.T) {
	c := &Client{config: &Config{
		Endpoint: "localhost:8080",
		Token:    "tok",
		Insecure: true,
		PprofURL: "",
		Service:  "svc",
	}}
	err := c.CaptureHeapProfile(context.Background())
	if err == nil {
		t.Error("expected error when no pprof server available")
	}
}

// TestCaptureCPUProfile_BadURL covers profile.go:38-40 — http.NewRequestWithContext error
// when pprofURL is an invalid URL.
func TestCaptureCPUProfile_BadURL(t *testing.T) {
	c := &Client{config: &Config{
		Endpoint: "localhost:8080",
		Token:    "tok",
		Insecure: true,
		PprofURL: "http://[invalid",
		Service:  "svc",
	}}
	err := c.CaptureCPUProfile(context.Background(), time.Second)
	if err == nil {
		t.Error("expected error for invalid pprof URL")
	}
}

// TestCaptureHeapProfile_BadURL covers profile.go:73-75 — http.NewRequestWithContext error
// when pprofURL is an invalid URL.
func TestCaptureHeapProfile_BadURL(t *testing.T) {
	c := &Client{config: &Config{
		Endpoint: "localhost:8080",
		Token:    "tok",
		Insecure: true,
		PprofURL: "http://[invalid",
		Service:  "svc",
	}}
	err := c.CaptureHeapProfile(context.Background())
	if err == nil {
		t.Error("expected error for invalid pprof URL")
	}
}

// TestUploadProfile_NetworkFailure covers profile.go:150-152 — http.Client.Do error
// when the backend server is unreachable.
func TestUploadProfile_NetworkFailure(t *testing.T) {
	cfg := &Config{
		Endpoint: "localhost:19996",
		Insecure: true,
		Token:    "tok",
		Service:  "svc",
	}
	err := uploadProfile(cfg, "cpu", nil, nil, []byte("data"))
	if err == nil {
		t.Error("expected error for unreachable server")
	}
}
