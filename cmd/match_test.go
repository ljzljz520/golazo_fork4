package cmd

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/0xjuanma/golazo/internal/api"
)

func TestRunMatch_MissingArg(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runMatch(&stdout, &stderr, cliFlags{mock: true, timeout: time.Second}, nil)
	if code != ExitInvalidArgs {
		t.Errorf("exit = %d, want %d", code, ExitInvalidArgs)
	}
	var env errEnvelope
	if err := json.Unmarshal(stderr.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal stderr: %v", err)
	}
	if env.Code != ErrCodeInvalidArgs {
		t.Errorf("code = %q, want %q", env.Code, ErrCodeInvalidArgs)
	}
}

func TestRunMatch_InvalidID(t *testing.T) {
	cases := []string{"abc", "0", "-1", ""}
	for _, arg := range cases {
		t.Run(arg, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runMatch(&stdout, &stderr, cliFlags{mock: true, timeout: time.Second}, []string{arg})
			if code != ExitInvalidArgs {
				t.Errorf("arg=%q exit = %d, want %d", arg, code, ExitInvalidArgs)
			}
		})
	}
}

func TestRunMatch_MockUnknownIDReturnsNotFound(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runMatch(&stdout, &stderr, cliFlags{mock: true, timeout: time.Second}, []string{"99999999"})
	if code != ExitNotFound {
		t.Errorf("exit = %d, want %d", code, ExitNotFound)
	}
	var env errEnvelope
	if err := json.Unmarshal(stderr.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal stderr: %v", err)
	}
	if env.Code != ErrCodeNotFound {
		t.Errorf("code = %q, want %q", env.Code, ErrCodeNotFound)
	}
}

func TestRunMatch_MockKnownIDReturnsDetails(t *testing.T) {
	// 2001 is the first mock live match ID (see internal/data/mock_live_matches.go).
	var stdout, stderr bytes.Buffer
	code := runMatch(&stdout, &stderr, cliFlags{mock: true, timeout: time.Second}, []string{"2001"})
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d. stderr=%s", code, ExitOK, stderr.String())
	}
	var env struct {
		Status string             `json:"status"`
		Count  int                `json:"count"`
		Data   []api.MatchDetails `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, stdout.String())
	}
	if env.Count != 1 || len(env.Data) != 1 {
		t.Fatalf("expected count=1 and one entry, got count=%d len=%d", env.Count, len(env.Data))
	}
	if env.Data[0].ID != 2001 {
		t.Errorf("data[0].ID = %d, want 2001", env.Data[0].ID)
	}
}

// TestRunMatch_OfflineEnvWritesErrorToStderr preserves the existing offline
// contract: GOLAZO_OFFLINE=1 refuses network access before any client work,
// regardless of the new location-index machinery.
func TestRunMatch_OfflineEnvWritesErrorToStderr(t *testing.T) {
	t.Setenv(EnvOffline, "1")

	var stdout, stderr bytes.Buffer
	code := runMatch(&stdout, &stderr, cliFlags{mock: false, timeout: time.Second}, []string{"12345"})

	if code != ExitOffline {
		t.Errorf("exit code = %d, want %d", code, ExitOffline)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout should be empty on offline error, got: %s", stdout.String())
	}
	var env errEnvelope
	if err := json.Unmarshal(stderr.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal stderr: %v\nraw: %s", err, stderr.String())
	}
	if env.Code != ErrCodeOffline {
		t.Errorf("error code = %q, want %q", env.Code, ErrCodeOffline)
	}
}

// TestRunMatch_TimeoutReportsTimeout ensures a deadline exceeded while
// resolving match details surfaces as the documented timeout envelope rather
// than a generic upstream error or a phantom empty success.
func TestRunMatch_TimeoutReportsTimeout(t *testing.T) {
	t.Setenv(EnvOffline, "")
	t.Setenv(EnvAgent, "")

	// 1ns timeout: the headless client context is deadline-exceeded before
	// the first HTTP call. The page/API fetch paths must propagate it and
	// the CLI maps it to ErrCodeTimeout.
	var stdout, stderr bytes.Buffer
	code := runMatch(&stdout, &stderr, cliFlags{mock: false, timeout: 1}, []string{"12345"})

	if code != ExitTimeout {
		t.Errorf("exit = %d, want %d (stderr=%s, stdout=%s)", code, ExitTimeout, stderr.String(), stdout.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout should be empty on timeout, got: %s", stdout.String())
	}
	var env errEnvelope
	if err := json.Unmarshal(stderr.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal stderr: %v\nraw: %s", err, stderr.String())
	}
	if env.Code != ErrCodeTimeout {
		t.Errorf("error code = %q, want %q", env.Code, ErrCodeTimeout)
	}
}

// TestRunMatch_OfflineMockStillServed confirms mock mode remains hermetic and
// keeps working under GOLAZO_OFFLINE (no index or network involvement).
func TestRunMatch_OfflineMockStillServed(t *testing.T) {
	t.Setenv(EnvOffline, "1")

	var stdout, stderr bytes.Buffer
	code := runMatch(&stdout, &stderr, cliFlags{mock: true, timeout: time.Second}, []string{"2001"})

	if code != ExitOK {
		t.Fatalf("exit = %d, want %d. stderr=%s", code, ExitOK, stderr.String())
	}
	if stdout.Len() == 0 {
		t.Error("stdout empty under offline+mock")
	}
}
