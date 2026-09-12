/*
Copyright 2025 The Scion Authors.
*/

package commands

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
)

func intPtr(i int) *int { return &i }

func TestClassifyExit(t *testing.T) {
	tests := []struct {
		name              string
		supervisedCode    int
		supervisorErr     error
		harnessCode       *int
		limitsExceeded    bool
		requestedShutdown bool
		wantCode          int
		wantCrash         bool
		wantLimits        bool
		wantMsg           string
	}{
		{
			name:           "clean exit code 0",
			supervisedCode: 0,
			wantCode:       0,
			wantCrash:      false,
		},
		{
			name:           "harness file reports non-zero while supervised child is 0 -> crash",
			supervisedCode: 0,
			harnessCode:    intPtr(42),
			wantCode:       42,
			wantCrash:      true,
			wantMsg:        "Agent crashed with exit code 42",
		},
		{
			name:           "harness file reports 0 while supervised child is 0 -> clean",
			supervisedCode: 0,
			harnessCode:    intPtr(0),
			wantCode:       0,
			wantCrash:      false,
		},
		{
			name:           "no harness file, supervised child non-zero -> crash (SIGKILL fallback)",
			supervisedCode: 137,
			wantCode:       137,
			wantCrash:      true,
			wantMsg:        "Agent crashed with exit code 137",
		},
		{
			name:           "limits exceeded via flag",
			supervisedCode: 0,
			limitsExceeded: true,
			wantCode:       handlers.ExitCodeLimitsExceeded,
			wantLimits:     true,
			wantCrash:      false,
		},
		{
			name:           "limits exceeded via child exit code",
			supervisedCode: handlers.ExitCodeLimitsExceeded,
			wantCode:       handlers.ExitCodeLimitsExceeded,
			wantLimits:     true,
			wantCrash:      false,
		},
		{
			name:           "supervisor error with zero code -> crash code 1",
			supervisedCode: 0,
			supervisorErr:  errors.New("boom"),
			wantCode:       1,
			wantCrash:      true,
			wantMsg:        "Agent crashed (supervisor error: boom)",
		},
		{
			name:           "signal-killed without requested shutdown is crash",
			supervisedCode: -1,
			wantCode:       -1,
			wantCrash:      true,
			wantMsg:        "Agent crashed with exit code -1",
		},
		{
			name:              "signal-killed with requested shutdown is clean stop",
			supervisedCode:    -1,
			requestedShutdown: true,
			wantCode:          0,
			wantCrash:         false,
		},
		{
			name:              "requested shutdown with non-signal exit code is still crash",
			supervisedCode:    1,
			requestedShutdown: true,
			wantCode:          1,
			wantCrash:         true,
			wantMsg:           "Agent crashed with exit code 1",
		},
		{
			name:              "requested shutdown preserves authoritative harness code 255",
			supervisedCode:    -1,
			harnessCode:       intPtr(255),
			requestedShutdown: true,
			wantCode:          255,
			wantCrash:         true,
			wantMsg:           "Agent crashed with exit code 255",
		},
		{
			name:              "harness code -1 with requested shutdown is clean stop",
			supervisedCode:    0,
			harnessCode:       intPtr(-1),
			requestedShutdown: true,
			wantCode:          0,
			wantCrash:         false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyExit(tc.supervisedCode, tc.supervisorErr, tc.harnessCode, tc.limitsExceeded, tc.requestedShutdown)
			if got.exitCode != tc.wantCode {
				t.Errorf("exitCode = %d, want %d", got.exitCode, tc.wantCode)
			}
			if got.isCrash != tc.wantCrash {
				t.Errorf("isCrash = %v, want %v", got.isCrash, tc.wantCrash)
			}
			if got.limitsExceeded != tc.wantLimits {
				t.Errorf("limitsExceeded = %v, want %v", got.limitsExceeded, tc.wantLimits)
			}
			if tc.wantMsg != "" && got.message != tc.wantMsg {
				t.Errorf("message = %q, want %q", got.message, tc.wantMsg)
			}
			if tc.wantMsg == "" && got.message != "" {
				t.Errorf("message = %q, want empty", got.message)
			}
		})
	}
}

func TestRunInitRequestedSIGTERMReturnsClassifiedExit(t *testing.T) {
	home := t.TempDir()
	marker := home + "/child-started"

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunInitRequestedSIGTERMHelper$")
	cmd.Env = make([]string, 0, len(os.Environ())+6)
	for _, env := range os.Environ() {
		if strings.HasPrefix(env, "SCION_") || strings.HasPrefix(env, "HOME=") {
			continue
		}
		cmd.Env = append(cmd.Env, env)
	}
	cmd.Env = append(cmd.Env,
		"GO_WANT_RUN_INIT_SIGTERM_HELPER=1",
		"HOME="+home,
		"SCION_HOOKS_DIR="+home+"/hooks",
		"SCION_GRACE_PERIOD=100ms",
		"SCION_TELEMETRY_ENABLED=false",
		"SCION_TEST_CHILD_STARTED="+marker,
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child did not start: %s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM to helper: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("runInit returned non-zero after requested SIGTERM: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "Child exited with code 0") {
		t.Fatalf("runInit did not log the classified exit code:\n%s", output.String())
	}
}

func TestRunInitRequestedSIGTERMHelper(t *testing.T) {
	if os.Getenv("GO_WANT_RUN_INIT_SIGTERM_HELPER") != "1" {
		return
	}

	code := runInit([]string{"sh", "-c", `touch "$SCION_TEST_CHILD_STARTED"; exec sleep 60`})
	os.Exit(code)
}

func TestReadHarnessExitCode(t *testing.T) {
	// Missing file -> nil.
	_ = os.Remove(state.HarnessExitCodeFile)
	if got := readHarnessExitCode(); got != nil {
		t.Errorf("expected nil for missing file, got %v", *got)
	}

	// Valid code.
	if err := os.WriteFile(state.HarnessExitCodeFile, []byte("137\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(state.HarnessExitCodeFile) })
	got := readHarnessExitCode()
	if got == nil || *got != 137 {
		t.Errorf("expected 137, got %v", got)
	}

	// Unparseable -> nil.
	if err := os.WriteFile(state.HarnessExitCodeFile, []byte("garbage"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readHarnessExitCode(); got != nil {
		t.Errorf("expected nil for garbage, got %v", *got)
	}
}
