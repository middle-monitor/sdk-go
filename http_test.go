package middlemonitor

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ── httpExceptionStore ────────────────────────────────────────────────────────

func TestHTTPExceptionStore_SetAndMessage(t *testing.T) {
	s := &httpExceptionStore{}
	s.Set(KeyExceptionMessage, "real cause")
	if s.message() != "real cause" {
		t.Errorf("want 'real cause', got %q", s.message())
	}
}

func TestHTTPExceptionStore_IgnoresOtherKeysAndTypes(t *testing.T) {
	s := &httpExceptionStore{}
	s.Set("other_key", "value")
	s.Set(KeyExceptionMessage, 42) // non-string ignored
	if s.message() != "" {
		t.Errorf("want empty message, got %q", s.message())
	}
}

// ── panicRecoveryHandler absorbs the re-panic from HTTPMiddleware so the test
// server doesn't crash (same role as panicRecoveryMiddleware for Echo).
func panicRecoveryHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ── HTTPMiddleware ────────────────────────────────────────────────────────────

func TestHTTPMiddleware_NilClient_BlockedAutoInit(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	// No token configured, so auto-init is a no-op and globalClient stays nil
	t.Setenv("MIDDLE_MONITOR_TOKEN", "")

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pong"))
	})

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	rec := httptest.NewRecorder()
	HTTPMiddleware(mux).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
}

func TestHTTPMiddleware_2xx(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	defer otlpSrv.Close()

	Init(NewConfig(otlpSrv.URL, "svc", "tok"))

	mux := http.NewServeMux()
	mux.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	rec := httptest.NewRecorder()
	HTTPMiddleware(mux).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
}

func TestHTTPMiddleware_4xx(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	defer otlpSrv.Close()

	Init(NewConfig(otlpSrv.URL, "svc", "tok"))

	mux := http.NewServeMux()
	mux.HandleFunc("/notfound", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})

	req := httptest.NewRequest(http.MethodGet, "/notfound", nil)
	rec := httptest.NewRecorder()
	HTTPMiddleware(mux).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
}

func TestHTTPMiddleware_5xx_JSONBody(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	backendSrv := startBackendErrorsServer(t)
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	mux := http.NewServeMux()
	mux.HandleFunc("/fail", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"db down"}`))
	})

	req := httptest.NewRequest(http.MethodGet, "/fail", nil)
	rec := httptest.NewRecorder()
	HTTPMiddleware(mux).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rec.Code)
	}
	time.Sleep(150 * time.Millisecond)
}

// TestHTTPMiddleware_ReportExceptionWithContext verifies the store path: a handler
// hiding the real error behind an empty 500 body still gets the real message into
// the Errors view when it calls ReportExceptionWithContext. This is the contract the
// backend api relies on for self-monitoring.
func TestHTTPMiddleware_ReportExceptionWithContext(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	received := make(chan appErrorPayload, 1)
	backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/v1/errors") {
			var p appErrorPayload
			body, _ := io.ReadAll(r.Body)
			if json.Unmarshal(body, &p) == nil {
				select {
				case received <- p:
				default:
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	mux := http.NewServeMux()
	mux.HandleFunc("/crash", func(w http.ResponseWriter, r *http.Request) {
		ReportExceptionWithContext(r.Context(), errors.New("real error message"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{}`))
	})

	req := httptest.NewRequest(http.MethodGet, "/crash", nil)
	rec := httptest.NewRecorder()
	HTTPMiddleware(mux).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rec.Code)
	}
	select {
	case p := <-received:
		if p.Message != "real error message" {
			t.Errorf("want message 'real error message', got %q", p.Message)
		}
	case <-time.After(2 * time.Second):
		t.Error("no error submitted to the backend Errors API")
	}
}

// An application that reports its own errors sets DisableHTTPErrorReporting so
// each failure is recorded once, with the real cause, instead of twice — the
// middleware would otherwise add an entry built from the generic response body.
func TestHTTPMiddleware_DisableHTTPErrorReporting(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	submitted := make(chan struct{}, 1)
	backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/v1/errors") {
			select {
			case submitted <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backendSrv.Close()

	cfg := NewConfig(backendSrv.URL, "svc", "tok")
	cfg.DisableHTTPErrorReporting = true
	Init(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/crash", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})

	req := httptest.NewRequest(http.MethodGet, "/crash", nil)
	rec := httptest.NewRecorder()
	HTTPMiddleware(mux).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rec.Code)
	}
	select {
	case <-submitted:
		t.Error("middleware submitted a 5xx error despite DisableHTTPErrorReporting")
	case <-time.After(500 * time.Millisecond):
	}
}

func TestHTTPMiddleware_NeverSampleRoute_2xx(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	defer otlpSrv.Close()

	cfg := NewConfig(otlpSrv.URL, "svc", "tok")
	cfg.Sampling.Traces.NeverSampleRoutes = []string{"/health"}
	Init(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	HTTPMiddleware(mux).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
}

func TestHTTPMiddleware_NeverSampleRoute_With5xx(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	backendSrv := startBackendErrorsServer(t)
	defer backendSrv.Close()

	cfg := NewConfig(backendSrv.URL, "svc", "tok")
	cfg.Sampling.Traces.NeverSampleRoutes = []string{"/health"}
	cfg.Sampling.Traces.AlwaysSampleErrors = true
	Init(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"db"}`))
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	HTTPMiddleware(mux).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rec.Code)
	}
	time.Sleep(150 * time.Millisecond)
}

func TestHTTPMiddleware_Panic_ErrorType(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	backendSrv := startBackendErrorsServer(t)
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	mux := http.NewServeMux()
	mux.HandleFunc("/panic-err", func(w http.ResponseWriter, r *http.Request) {
		panic(errors.New("panic error"))
	})

	req := httptest.NewRequest(http.MethodGet, "/panic-err", nil)
	rec := httptest.NewRecorder()
	panicRecoveryHandler(HTTPMiddleware(mux)).ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

func TestHTTPMiddleware_Panic_StringType(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	backendSrv := startBackendErrorsServer(t)
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	mux := http.NewServeMux()
	mux.HandleFunc("/panic-str", func(w http.ResponseWriter, r *http.Request) {
		panic("string panic")
	})

	req := httptest.NewRequest(http.MethodGet, "/panic-str", nil)
	rec := httptest.NewRecorder()
	panicRecoveryHandler(HTTPMiddleware(mux)).ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

func TestHTTPMiddleware_Panic_OtherType(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	backendSrv := startBackendErrorsServer(t)
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	mux := http.NewServeMux()
	mux.HandleFunc("/panic-other", func(w http.ResponseWriter, r *http.Request) {
		panic(42)
	})

	req := httptest.NewRequest(http.MethodGet, "/panic-other", nil)
	rec := httptest.NewRecorder()
	panicRecoveryHandler(HTTPMiddleware(mux)).ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

func TestHTTPMiddleware_Panic_NoSpan(t *testing.T) {
	// Panic on a route where sampling=0 (no span created) → middleware creates an error span
	resetGlobalState()
	defer resetGlobalState()

	backendSrv := startBackendErrorsServer(t)
	defer backendSrv.Close()

	cfg := NewConfig(backendSrv.URL, "svc", "tok")
	cfg.Sampling.Traces.Percentage = 0
	cfg.Sampling.Traces.AlwaysSampleErrors = false
	Init(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/no-span", func(w http.ResponseWriter, r *http.Request) {
		panic(errors.New("panic no span"))
	})

	req := httptest.NewRequest(http.MethodGet, "/no-span", nil)
	rec := httptest.NewRecorder()
	panicRecoveryHandler(HTTPMiddleware(mux)).ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

func TestHTTPMiddleware_WithRequestBody(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	backendSrv := startBackendErrorsServer(t)
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"fail"}`))
	})

	body := strings.NewReader(`{"user":"test"}`)
	req := httptest.NewRequest(http.MethodPost, "/api", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	HTTPMiddleware(mux).ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

func TestHTTPMiddleware_WithLargeRequestBody(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	backendSrv := startBackendErrorsServer(t)
	defer backendSrv.Close()

	Init(NewConfig(backendSrv.URL, "svc", "tok"))

	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"fail"}`))
	})

	// Body > 10000 bytes → middleware truncates to 10000
	largeBody := strings.NewReader(strings.Repeat("x", 11000))
	req := httptest.NewRequest(http.MethodPost, "/api", largeBody)
	rec := httptest.NewRecorder()
	HTTPMiddleware(mux).ServeHTTP(rec, req)

	time.Sleep(150 * time.Millisecond)
}

func TestHTTPMiddleware_TraceContextPropagation(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	defer otlpSrv.Close()

	Init(NewConfig(otlpSrv.URL, "svc", "tok"))

	mux := http.NewServeMux()
	mux.HandleFunc("/trace", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("traced"))
	})

	req := httptest.NewRequest(http.MethodGet, "/trace", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	rec := httptest.NewRecorder()
	HTTPMiddleware(mux).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
}
