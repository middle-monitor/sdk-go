package main

import (
	"errors"
	"log"

	middlemonitor "github.com/middle-monitor/sdk-go"
)

func main() {
	err := middlemonitor.InitWithConfig(
		"http://localhost:4318", // OTLP endpoint (OTEL Collector or Middle-Monitor backend)
		"example-service",
		"", // token (optional in dev)
	)
	if err != nil {
		log.Fatalf("Failed to initialize Middle-Monitor: %v", err)
	}

	client := middlemonitor.GetGlobalClient()
	if client == nil {
		log.Fatal("Client is nil")
	}

	// Report a simple error
	exampleErr := errors.New("something went wrong")
	client.ReportError(exampleErr)

	// Report an error with explicit file and line
	client.ReportErrorWithDetails(exampleErr, "/path/to/file.go", 42)

	// Report a named error
	client.ReportCustomError(
		"DatabaseError",
		"Failed to connect to database",
		"/path/to/db.go",
		123,
	)

	// Capture panics (re-panics after reporting)
	defer client.CapturePanic()
	riskyFunction()
}

func riskyFunction() {
	panic("this will be reported before re-panicking")
}
