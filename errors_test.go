package middlemonitor

import (
	"errors"
	"testing"
)

func TestSentinelErrors_NotNil(t *testing.T) {
	for _, e := range []error{ErrNotInitialized, ErrConfigMissing, ErrConfigRequired} {
		if e == nil {
			t.Errorf("sentinel error must not be nil: %v", e)
		}
	}
}

func TestSentinelErrors_Distinct(t *testing.T) {
	if errors.Is(ErrNotInitialized, ErrConfigMissing) {
		t.Error("ErrNotInitialized should not be ErrConfigMissing")
	}
	if errors.Is(ErrNotInitialized, ErrConfigRequired) {
		t.Error("ErrNotInitialized should not be ErrConfigRequired")
	}
	if errors.Is(ErrConfigMissing, ErrConfigRequired) {
		t.Error("ErrConfigMissing should not be ErrConfigRequired")
	}
}

func TestSentinelErrors_Messages(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{ErrNotInitialized, "client not initialized"},
		{ErrConfigMissing, "config not initialized"},
		{ErrConfigRequired, "endpoint and token required"},
	}
	for _, tc := range cases {
		if tc.err.Error() != tc.want {
			t.Errorf("%v.Error() = %q, want %q", tc.err, tc.err.Error(), tc.want)
		}
	}
}
