# Middle-Monitor SDK for Go

Drop-in SDK for error reporting with Middle-Monitor. Auto-configured via environment variables, with automatic error and panic capture.

## Installation

```bash
go get github.com/middle-monitor/sdk-go
```

## Automatic configuration (recommended)

Set the environment variables and the SDK initializes itself:

```bash
export MIDDLE_MONITOR_API_URL="https://api.middlemonitor.io"
export MIDDLE_MONITOR_TOKEN="your_token"
export MIDDLE_MONITOR_SERVICE="my-service"
```

## Usage with Echo (recommended)

```go
package main

import (
    "github.com/labstack/echo/v4"
    "github.com/middle-monitor/sdk-go"
)

func main() {
    e := echo.New()

    // One line to enable automatic capture
    e.Use(middlemonitor.EchoMiddleware())

    e.GET("/", func(c echo.Context) error {
        return fmt.Errorf("an error") // Captured automatically
    })

    e.Logger.Fatal(e.Start(":8080"))
}
```

## Usage with Gin

```go
package main

import (
    "github.com/gin-gonic/gin"
    "github.com/middle-monitor/sdk-go"
)

func main() {
    r := gin.Default()

    // One line to enable automatic capture
    r.Use(middlemonitor.GinMiddleware())

    r.GET("/", func(c *gin.Context) {
        panic("a panic") // Captured automatically
    })

    r.Run(":8080")
}
```

## Panic capture at startup

```go
package main

import (
    "github.com/middle-monitor/sdk-go"
)

func main() {
    defer middlemonitor.CapturePanicGlobal()

    panic("a startup panic") // Captured automatically
}
```

## Manual setup (optional)

```go
package main

import (
    "github.com/middle-monitor/sdk-go"
)

func main() {
    middlemonitor.InitWithConfig(
        "https://api.middlemonitor.io",
        "my-service",
        "your_token",
    )

    err := doSomething()
    if err != nil {
        middlemonitor.ReportError(err)
    }
}
```

## Logging

Send structured logs to Middle-Monitor:

```go
// Buffered (async, batched)
middlemonitor.Log(ctx, middlemonitor.LogLevelINFO, "Message", map[string]string{
    "key": "value",
})

// Immediate (sync)
middlemonitor.LogSync(ctx, middlemonitor.LogLevelERROR, "Critical error", nil)

// Flush before shutdown
middlemonitor.FlushLogs(ctx)
```

Levels: `LogLevelDEBUG`, `LogLevelINFO`, `LogLevelWARN`, `LogLevelERROR`, `LogLevelFATAL`, `LogLevelPANIC`.

## Global functions

- `middlemonitor.ReportError(err)` — report an error
- `middlemonitor.ReportErrorWithDetails(err, file, line)` — report with file/line details
- `middlemonitor.CapturePanicGlobal()` — capture a panic (use with defer)
- `middlemonitor.Log(ctx, level, message, attrs)` — send a buffered log
- `middlemonitor.LogSync(ctx, level, message, attrs)` — send a log immediately
- `middlemonitor.FlushLogs(ctx)` — flush the log buffer before shutdown

## Environment variables

| Variable | Description | Required | Default |
|---|---|---|---|
| `MIDDLE_MONITOR_API_URL` | Middle-Monitor API URL (e.g. `http://localhost:8081`) | Recommended | `http://localhost:8080` |
| `MIDDLE_MONITOR_TOKEN` | Authentication token | Recommended | — |
| `MIDDLE_MONITOR_SERVICE` | Service name | No | `"unknown"` |

Only **server errors (5xx)** and **panics** are reported. **4xx** responses (401, 404, etc.) are treated as client errors and are not sent.

## Troubleshooting

### "failed to upload metrics" / "connection refused"

The SDK sends traces, logs, and metrics to the configured URL (OTLP: `/v1/traces`, `/v1/logs`, `/v1/metrics`). If you see:

```
Post "http://localhost:8080/v1/metrics": dial tcp ... connection refused
```

Nothing is listening on the configured port. Set the correct backend URL:

```bash
export MIDDLE_MONITOR_API_URL="http://localhost:8081"
```

In Docker Compose, use the service hostname instead of `localhost` (e.g. `http://backend:8080`).