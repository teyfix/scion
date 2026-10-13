package commands

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

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

	code := RunInit([]string{"sh", "-c", `touch "$SCION_TEST_CHILD_STARTED"; exec sleep 60`}, InitRunOptions{})
	os.Exit(code)
}
