package middlemonitor

import "errors"

var (
	// ErrNotInitialized is returned when an operation is attempted before Init is called.
	ErrNotInitialized = errors.New("client not initialized")

	// ErrConfigMissing is returned when the global config is not yet available.
	ErrConfigMissing = errors.New("config not initialized")

	// ErrConfigRequired is returned when a feature requires an endpoint and token that are not set.
	ErrConfigRequired = errors.New("endpoint and token required")
)
