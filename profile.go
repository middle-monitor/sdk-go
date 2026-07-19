package middlemonitor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"runtime"
	"runtime/pprof"
	"strings"
	"time"
)

// CaptureCPUProfile collects a CPU profile for the given duration and uploads it
// to the Middle-Monitor API. It profiles this process directly; set
// Config.PprofURL to scrape an external pprof server instead.
func (c *Client) CaptureCPUProfile(ctx context.Context, duration time.Duration) error {
	cfg := c.config
	if cfg == nil || cfg.Endpoint == "" || cfg.Token == "" {
		return ErrConfigRequired
	}
	seconds := int(duration.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	if seconds > 120 {
		seconds = 120
	}

	var data []byte
	var err error
	if cfg.PprofURL != "" {
		path := fmt.Sprintf("/debug/pprof/profile?seconds=%d", seconds)
		data, err = fetchPprof(ctx, cfg.PprofURL, path, time.Duration(seconds)*time.Second+10*time.Second)
	} else {
		data, err = captureCPUInProcess(ctx, time.Duration(seconds)*time.Second)
	}
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return ErrProfileEmpty
	}
	return uploadProfile(cfg, "cpu", &seconds, nil, data)
}

// CaptureHeapProfile collects a heap profile and uploads it to the
// Middle-Monitor API. It profiles this process directly; set Config.PprofURL to
// scrape an external pprof server instead.
func (c *Client) CaptureHeapProfile(ctx context.Context) error {
	cfg := c.config
	if cfg == nil || cfg.Endpoint == "" || cfg.Token == "" {
		return ErrConfigRequired
	}

	var data []byte
	var err error
	if cfg.PprofURL != "" {
		data, err = fetchPprof(ctx, cfg.PprofURL, "/debug/pprof/heap", 30*time.Second)
	} else {
		data, err = captureHeapInProcess()
	}
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return ErrProfileEmpty
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	mb := float64(m.HeapInuse) / (1024 * 1024)
	return uploadProfile(cfg, "heap", nil, &mb, data)
}

// captureCPUInProcess runs the CPU profiler for the given duration. Only one CPU
// profile can be active per process, so a concurrent call fails rather than
// silently returning a truncated profile.
func captureCPUInProcess(ctx context.Context, duration time.Duration) ([]byte, error) {
	var buf bytes.Buffer
	if err := pprof.StartCPUProfile(&buf); err != nil {
		return nil, fmt.Errorf("start cpu profile: %w", ErrProfileInProgress)
	}
	select {
	case <-time.After(duration):
	case <-ctx.Done():
	}
	pprof.StopCPUProfile()
	return buf.Bytes(), nil
}

// captureHeapInProcess writes the current heap profile. The GC runs first so the
// profile reflects live objects rather than uncollected garbage.
func captureHeapInProcess() ([]byte, error) {
	runtime.GC()
	var buf bytes.Buffer
	if err := pprof.WriteHeapProfile(&buf); err != nil {
		return nil, fmt.Errorf("write heap profile: %w", ErrProfileRead)
	}
	return buf.Bytes(), nil
}

// fetchPprof scrapes a profile from an external pprof HTTP server.
func fetchPprof(ctx context.Context, baseURL, path string, timeout time.Duration) ([]byte, error) {
	url := strings.TrimSuffix(baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create pprof request: %w", ErrPprofRequest)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch profile: %w", ErrProfileFetch)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &HTTPStatusError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read profile: %w", ErrProfileRead)
	}
	return data, nil
}

// apiBase returns the HTTP base URL for the Middle-Monitor API.
func apiBase(cfg *Config) string {
	base := cfg.Endpoint
	if base != "" && !strings.Contains(cfg.Endpoint, "://") {
		if cfg.Insecure {
			base = "http://" + base
		} else {
			base = "https://" + base
		}
	}
	return strings.TrimSuffix(base, "/")
}

func uploadProfile(cfg *Config, profileType string, durationSeconds *int, memoryMB *float64, data []byte) error {
	base := apiBase(cfg)
	url := base + "/api/v1/profiles"
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	_ = w.WriteField("profile_type", profileType)
	_ = w.WriteField("service", cfg.Service)
	if durationSeconds != nil {
		_ = w.WriteField("duration_seconds", fmt.Sprintf("%d", *durationSeconds))
	}
	if memoryMB != nil {
		_ = w.WriteField("memory_mb", fmt.Sprintf("%.2f", *memoryMB))
	}
	part, err := w.CreateFormFile("profile", "profile.pprof")
	if err != nil {
		return fmt.Errorf("create form: %w", ErrMultipartBuild)
	}
	if _, err = part.Write(data); err != nil {
		return fmt.Errorf("write form: %w", ErrMultipartBuild)
	}
	contentType := w.FormDataContentType()
	if err = w.Close(); err != nil {
		return fmt.Errorf("close form: %w", ErrMultipartBuild)
	}

	req, err := http.NewRequest("POST", url, &buf)
	if err != nil {
		return fmt.Errorf("create request: %w", ErrUploadRequest)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("upload: %w", ErrProfileUpload)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return &HTTPStatusError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	slog.Info("uploaded profile", "type", profileType, "bytes", len(data))
	return nil
}
