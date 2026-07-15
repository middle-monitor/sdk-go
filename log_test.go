package middlemonitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// newLogTestConfig returns a Config pointing at the given test server.
func newLogTestConfig(serverURL string) *Config {
	cfg := NewConfig(serverURL, "test-svc", "tok")
	return cfg
}

// ── buildLogRecord ────────────────────────────────────────────────────────────

func TestBuildLogRecord_AllLevels(t *testing.T) {
	cfg := newTestConfig()
	levels := []LogLevel{
		LogLevelDEBUG, LogLevelINFO, LogLevelWARN,
		LogLevelERROR, LogLevelFATAL, LogLevelPANIC,
	}
	for _, lvl := range levels {
		rec := buildLogRecord(lvl, "test message", map[string]string{"k": "v"}, cfg)
		if rec == nil {
			t.Errorf("level %s: got nil record", lvl)
		}
		if rec.SeverityText != string(lvl) {
			t.Errorf("level %s: want SeverityText=%s, got %s", lvl, lvl, rec.SeverityText)
		}
		if rec.Body == nil {
			t.Errorf("level %s: body should not be nil", lvl)
		}
	}
}

func TestBuildLogRecord_NoAttrs(t *testing.T) {
	cfg := newTestConfig()
	rec := buildLogRecord(LogLevelINFO, "msg", nil, cfg)
	if rec == nil {
		t.Fatal("expected non-nil record")
	}
	if len(rec.Attributes) != 0 {
		t.Errorf("want 0 attrs, got %d", len(rec.Attributes))
	}
}

// ── logLevelToSeverity ────────────────────────────────────────────────────────

func TestLogLevelToSeverity_AllLevels(t *testing.T) {
	cases := []struct {
		level    LogLevel
		wantNot  logspb.SeverityNumber
		wantZero bool
	}{
		{LogLevelDEBUG, logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, false},
		{LogLevelINFO, logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, false},
		{LogLevelWARN, logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, false},
		{LogLevelERROR, logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, false},
		{LogLevelFATAL, logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, false},
		{LogLevelPANIC, logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, false},
	}
	for _, tc := range cases {
		sev := logLevelToSeverity(tc.level)
		if sev == logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED {
			t.Errorf("level %s: unexpected UNSPECIFIED severity", tc.level)
		}
	}
}

func TestLogLevelToSeverity_Default(t *testing.T) {
	sev := logLevelToSeverity(LogLevel("UNKNOWN"))
	if sev != logspb.SeverityNumber_SEVERITY_NUMBER_INFO {
		t.Errorf("want INFO for unknown level, got %v", sev)
	}
}

// ── sendLogs ─────────────────────────────────────────────────────────────────

func TestSendLogs_Empty(t *testing.T) {
	cfg := newTestConfig()
	err := sendLogs(context.Background(), nil, cfg)
	if err != nil {
		t.Errorf("empty records should be no-op, got: %v", err)
	}
}

func TestSendLogs_Success(t *testing.T) {
	received := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := newLogTestConfig(srv.URL)
	cfg.Insecure = true
	rec := buildLogRecord(LogLevelERROR, "test error", nil, cfg)

	err := sendLogs(context.Background(), []*logspb.LogRecord{rec}, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Error("test server did not receive the request")
	}
}

func TestSendLogs_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := newLogTestConfig(srv.URL)
	cfg.Insecure = true
	rec := buildLogRecord(LogLevelERROR, "err", nil, cfg)

	err := sendLogs(context.Background(), []*logspb.LogRecord{rec}, cfg)
	if err == nil {
		t.Error("expected error for non-200 status")
	}
}

func TestSendLogs_BadURL(t *testing.T) {
	cfg := newTestConfig()
	cfg.Endpoint = "not-a-host:99999"
	cfg.Insecure = true
	rec := buildLogRecord(LogLevelERROR, "err", nil, cfg)
	err := sendLogs(context.Background(), []*logspb.LogRecord{rec}, cfg)
	if err == nil {
		t.Error("expected error for unreachable host")
	}
}

// ── flushLogs ─────────────────────────────────────────────────────────────────

func TestFlushLogs_NilClient(t *testing.T) {
	cfg := newTestConfig()
	// Should not panic
	flushLogs(context.Background(), nil, cfg)
}

func TestFlushLogs_NilConfig(t *testing.T) {
	// Should not panic
	flushLogs(context.Background(), &Client{}, nil)
}

func TestFlushLogs_EmptyBuffer(t *testing.T) {
	cfg := newTestConfig()
	client := &Client{}
	logBufferMu.Lock()
	logBuffer = nil
	logBufferMu.Unlock()
	// Should not panic and should be a no-op
	flushLogs(context.Background(), client, cfg)
}

func TestFlushLogs_WithRecords_ServerDown(t *testing.T) {
	cfg := newTestConfig()
	cfg.Endpoint = "localhost:19999" // nothing listening
	cfg.Insecure = true
	client := &Client{}

	logBufferMu.Lock()
	logBuffer = []*logspb.LogRecord{
		buildLogRecord(LogLevelERROR, "err", nil, cfg),
	}
	logBufferMu.Unlock()

	// Should not panic — records are re-added on failure
	flushLogs(context.Background(), client, cfg)

	logBufferMu.Lock()
	logBuffer = nil
	logBufferMu.Unlock()
}

// ── startLogFlusher ───────────────────────────────────────────────────────────

func TestStartLogFlusher_Idempotent(t *testing.T) {
	// Reset the flusherOnce so this test can exercise the code path
	flusherOnce = sync.Once{}
	startLogFlusher()
	// Calling again should be safe (idempotent)
	startLogFlusher()
}

// ── Log / LogSync / FlushLogs (global API, nil client path) ──────────────────

func TestLog_NoClient(t *testing.T) {
	resetGlobalState()
	// Should not panic when no global client is set
	Log(context.Background(), LogLevelINFO, "msg", nil)
}

func TestLogSync_NoClient(t *testing.T) {
	resetGlobalState()
	// Block auto-init so GetGlobalClient() returns nil
	initOnce.Do(func() {})
	defer resetGlobalState()
	err := LogSync(context.Background(), LogLevelINFO, "msg", nil)
	if err != ErrNotInitialized {
		t.Errorf("want ErrNotInitialized, got %v", err)
	}
}

func TestLogSync_NoConfig(t *testing.T) {
	resetGlobalState()
	// Block auto-init, then set a client with no config so GetGlobalConfig() returns nil
	initOnce.Do(func() {})
	globalClient = &Client{}
	defer resetGlobalState()
	err := LogSync(context.Background(), LogLevelINFO, "msg", nil)
	if err != ErrConfigMissing {
		t.Errorf("want ErrConfigMissing, got %v", err)
	}
}

func TestFlushLogs_GlobalAPI(t *testing.T) {
	resetGlobalState()
	// Should not panic
	FlushLogs(context.Background())
}

// TestStartLogFlusher_GoroutineBody covers log.go:35-37 — the goroutine body that
// fires on each ticker tick. We shorten the interval so the test is fast.
func TestStartLogFlusher_GoroutineBody(t *testing.T) {
	flusherOnce = sync.Once{}
	old := logFlushInterval
	logFlushInterval = 20 * time.Millisecond
	defer func() {
		logFlushInterval = old
		flusherOnce = sync.Once{}
	}()
	startLogFlusher()
	time.Sleep(80 * time.Millisecond)
}

// TestLog_NilClient_Blocked covers log.go:46-48 — early return when client is nil.
func TestLog_NilClient_Blocked(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	initOnce.Do(func() {}) // block auto-init so client stays nil
	Log(context.Background(), LogLevelINFO, "msg", nil)
}

// TestLog_NilConfig_Blocked covers log.go:50-52 — early return when config is nil.
func TestLog_NilConfig_Blocked(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	initOnce.Do(func() {}) // block auto-init
	globalClient = &Client{}
	Log(context.Background(), LogLevelINFO, "msg", nil)
}

// TestLog_ShouldFlush covers log.go:64-66 — the flush goroutine trigger when the
// buffer reaches logBufferSize entries.
func TestLog_ShouldFlush(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NewConfig(srv.URL, "svc", "tok")
	cfg.Insecure = true
	Init(cfg)

	logBufferMu.Lock()
	logBuffer = nil
	logBufferMu.Unlock()

	for i := 0; i < logBufferSize+1; i++ {
		Log(context.Background(), LogLevelINFO, "fill", nil)
	}
	time.Sleep(100 * time.Millisecond)
}

// TestLogSync_Success covers log.go:79-80 — the happy path of LogSync when both
// client and config are initialized.
func TestLogSync_Success(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NewConfig(srv.URL, "svc", "tok")
	cfg.Insecure = true
	if err := Init(cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}

	err := LogSync(context.Background(), LogLevelINFO, "hello", nil)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestSendLogs_BadURL_InvalidHost covers log.go:193-195 — http.NewRequestWithContext
// error when the endpoint produces an invalid URL.
func TestSendLogs_BadURL_InvalidHost(t *testing.T) {
	cfg := newTestConfig()
	cfg.Endpoint = "[invalid"
	cfg.Insecure = true
	rec := buildLogRecord(LogLevelERROR, "err", nil, cfg)
	err := sendLogs(context.Background(), []*logspb.LogRecord{rec}, cfg)
	if err == nil {
		t.Error("expected error for invalid endpoint URL")
	}
}
