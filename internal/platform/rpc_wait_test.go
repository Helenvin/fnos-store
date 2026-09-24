//go:build linux

package platform

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// waitFakeDaemon serves /rpc/v1/common/status, replaying the scripted responses
// in order and repeating the last one once the script runs out.
func waitFakeDaemon(t *testing.T, statusScript []string) *int {
	t.Helper()
	var mu sync.Mutex
	calls := new(int)
	mux := http.NewServeMux()
	mux.HandleFunc(routeCommonStatus, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		n := *calls
		*calls++
		mu.Unlock()
		if n >= len(statusScript) {
			n = len(statusScript) - 1
		}
		_, _ = w.Write([]byte(statusScript[n]))
	})
	startFakeDaemon(t, mux)
	return calls
}

// Losing sight of a task is NOT the task failing. The daemon answers an unknown
// taskId with code 0 and status 5 (measured on fnOS 1.2.0505), which used to
// fall through to the generic branch and surface as the bare, actionless string
// "升级失败: 状态 5". The operation may in fact have SUCCEEDED — the daemon reaps
// finished tasks, and an fnOS system update restarts it outright — so this must
// be reported as an unknown outcome, never as a failure.
func TestWaitTaskUnknownTaskIsIndeterminate(t *testing.T) {
	// Given a daemon that no longer knows the task
	waitFakeDaemon(t, []string{`{"code":0,"data":{"status":5,"taskId":""}}`})
	a := NewLinuxAppCenter()

	// When the task is awaited
	err := a.waitTask(context.Background(), "task-1", "升级", nil)

	// Then the outcome is indeterminate, not a failure
	if !errors.Is(err, ErrTaskOutcomeUnknown) {
		t.Fatalf("err = %v, want ErrTaskOutcomeUnknown", err)
	}
	if strings.Contains(err.Error(), "状态 5") {
		t.Errorf("err = %q still reports the raw status instead of an actionable message", err)
	}
}

// A status poll is observational: failing to READ it says nothing about the
// task. StageFpk already rides out the daemon's transient 10050 / TRPC blips;
// the task poll covers far more wall-clock time and needs it at least as much.
func TestWaitTaskRidesOutTransientPollFailures(t *testing.T) {
	// Given a daemon that blips twice, then reports success
	calls := waitFakeDaemon(t, []string{
		`{"code":10050,"msg":"failed to get volume info: TRPC read timeout"}`,
		`{"code":10050,"msg":"failed to get volume info: TRPC read timeout"}`,
		`{"code":0,"data":{"status":2}}`,
	})
	a := NewLinuxAppCenter()

	// When the task is awaited
	if err := a.waitTask(context.Background(), "task-1", "升级", nil); err != nil {
		t.Fatalf("waitTask: %v — a transient status poll must not fail the task", err)
	}

	// Then it polled through the blips rather than aborting on the first
	if *calls != 3 {
		t.Errorf("status polled %d times, want 3", *calls)
	}
}

// A status the daemon reports that we do not recognise is still a definite
// terminal answer from the daemon — unlike an unknown task — so it must fail
// rather than be papered over as indeterminate.
func TestWaitTaskUnrecognisedStatusFails(t *testing.T) {
	// Given a daemon reporting an unmodelled terminal status
	waitFakeDaemon(t, []string{`{"code":0,"data":{"status":3,"message":"install hook failed"}}`})
	a := NewLinuxAppCenter()

	// When the task is awaited
	err := a.waitTask(context.Background(), "task-1", "安装", nil)

	// Then it is a failure carrying the daemon's own detail
	if err == nil {
		t.Fatal("err = nil, want a failure")
	}
	if errors.Is(err, ErrTaskOutcomeUnknown) {
		t.Errorf("err = %v, must not be indeterminate — the daemon gave a definite answer", err)
	}
	if !strings.Contains(err.Error(), "install hook failed") {
		t.Errorf("err = %q, want the daemon's own detail", err)
	}
}

// Cancelling the observer (e.g. the user closes the tab) does not cancel the
// daemon's work, so it cannot be reported as a failure either.
func TestWaitTaskCancellationIsIndeterminate(t *testing.T) {
	// Given a task that never finishes
	waitFakeDaemon(t, []string{`{"code":0,"data":{"status":1}}`})
	a := NewLinuxAppCenter()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// When the caller has already gone away
	err := a.waitTask(ctx, "task-1", "升级", nil)

	// Then the outcome is unknown, not failed
	if !errors.Is(err, ErrTaskOutcomeUnknown) {
		t.Fatalf("err = %v, want ErrTaskOutcomeUnknown", err)
	}
}

// removeStagedPackage runs as root, so it must only ever touch something that
// actually looks like the daemon's staging directory.
func TestRemoveStagedPackageOnlyTouchesStagingPaths(t *testing.T) {
	root := t.TempDir()

	staged := filepath.Join(root, "appcenter-downloads", "openlist-4.2.5-tpk")
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	// Paths that must survive: wrong parent, wrong suffix, and a relative path.
	survivors := []string{
		filepath.Join(root, "appcenter-downloads", "not-staging"),
		filepath.Join(root, "elsewhere", "openlist-4.2.5-tpk"),
	}
	for _, p := range survivors {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	removeStagedPackage(staged)
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("staged dir still present, want removed")
	}

	for _, p := range survivors {
		removeStagedPackage(p)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed; only .../appcenter-downloads/*-tpk may be touched", p)
		}
	}

	// Empty and relative paths must be no-ops rather than panics or surprises.
	removeStagedPackage("")
	removeStagedPackage("appcenter-downloads/rel-tpk")
}

// writeListCLI installs a fake appcenter-cli whose `list` output is the given
// table, so task verifiers can be exercised through the real parse path.
func writeListCLI(t *testing.T, table string) string {
	t.Helper()
	script := "#!/bin/sh\ncat <<'TABLE'\n" + table + "\nTABLE\n"
	path := filepath.Join(t.TempDir(), "appcenter-cli")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write list cli: %v", err)
	}
	return path
}

const jellyfinInstalledAtTarget = `│ APP NAME │ DISPLAY NAME │ VERSION │ STATUS │ DEPENDENCY APPS │
│ jellyfin │ Jellyfin     │ 12.1    │ running│                 │`

const jellyfinInstalledAtOldVersion = `│ APP NAME │ DISPLAY NAME │ VERSION │ STATUS │ DEPENDENCY APPS │
│ jellyfin │ Jellyfin     │ 12.0    │ running│                 │`

// When the daemon no longer knows a task but the installed list shows the app
// at the upgrade target, the upgrade DID complete — the poll merely lost sight
// of it. Report success instead of an unknowable error (issue #301: a user's
// Jellyfin upgrade finished, the server started, and the store still cried
// "app center 已不再持有该升级任务").
func TestWaitTaskUnknownTaskSettledByFinalState(t *testing.T) {
	// Given a daemon that no longer knows the task
	waitFakeDaemon(t, []string{`{"code":0,"data":{"status":5,"taskId":""}}`})
	a := &LinuxAppCenter{CLIPath: writeListCLI(t, jellyfinInstalledAtTarget)}

	// When the upgrade task is awaited with a postcondition
	err := a.waitTask(context.Background(), "task-1", "升级", a.verifyAppAtVersion("jellyfin", "12.1"))

	// Then the final state settles it as completed
	if err != nil {
		t.Fatalf("err = %v, want nil — final state confirms the upgrade", err)
	}
}

// A revision suffix on the target must not defeat the check: staging reports
// fpk_version (with -rN) while the daemon's list reports the manifest version.
func TestWaitTaskUnknownTaskSettledDespiteRevisionSuffix(t *testing.T) {
	waitFakeDaemon(t, []string{`{"code":0,"data":{"status":5,"taskId":""}}`})
	a := &LinuxAppCenter{CLIPath: writeListCLI(t, jellyfinInstalledAtTarget)}

	if err := a.waitTask(context.Background(), "task-1", "升级", a.verifyAppAtVersion("jellyfin", "12.1-r2")); err != nil {
		t.Fatalf("err = %v, want nil — -rN targets must compare by base version", err)
	}
}

// When the installed list contradicts completion — the app is still at the OLD
// version — the honest answer is still ErrTaskOutcomeUnknown (the task may
// have died mid-flight with a restart), but now carrying the evidence.
func TestWaitTaskUnknownTaskContradictedByFinalState(t *testing.T) {
	waitFakeDaemon(t, []string{`{"code":0,"data":{"status":5,"taskId":""}}`})
	a := &LinuxAppCenter{CLIPath: writeListCLI(t, jellyfinInstalledAtOldVersion)}

	err := a.waitTask(context.Background(), "task-1", "升级", a.verifyAppAtVersion("jellyfin", "12.1"))
	if !errors.Is(err, ErrTaskOutcomeUnknown) {
		t.Fatalf("err = %v, want ErrTaskOutcomeUnknown — a contradicted postcondition is not a proven failure", err)
	}
	if !strings.Contains(err.Error(), "12.0") {
		t.Errorf("err = %q, want it to carry the observed current version as evidence", err)
	}
}

// Install: registered in the list → settled as completed even though the task
// handle was reaped before the first poll.
func TestWaitTaskInstallSettledByRegistration(t *testing.T) {
	waitFakeDaemon(t, []string{`{"code":0,"data":{"status":5,"taskId":""}}`})
	a := &LinuxAppCenter{CLIPath: writeListCLI(t, jellyfinInstalledAtTarget)}

	if err := a.waitTask(context.Background(), "task-1", "安装", a.verifyAppRegistered("jellyfin")); err != nil {
		t.Fatalf("err = %v, want nil — the app is registered", err)
	}
}

// Uninstall: gone from the list → settled as completed.
func TestWaitTaskUninstallSettledByAbsence(t *testing.T) {
	waitFakeDaemon(t, []string{`{"code":0,"data":{"status":5,"taskId":""}}`})
	a := &LinuxAppCenter{CLIPath: writeListCLI(t, jellyfinInstalledAtTarget)}

	if err := a.waitTask(context.Background(), "task-1", "卸载", a.verifyAppAbsent("picoclaw")); err != nil {
		t.Fatalf("err = %v, want nil — picoclaw is gone from the list", err)
	}
}

// Uninstall contradicted: the app is still registered → unknown outcome with
// evidence, never a bare guess either way.
func TestWaitTaskUninstallContradictedByPresence(t *testing.T) {
	waitFakeDaemon(t, []string{`{"code":0,"data":{"status":5,"taskId":""}}`})
	a := &LinuxAppCenter{CLIPath: writeListCLI(t, jellyfinInstalledAtTarget)}

	err := a.waitTask(context.Background(), "task-1", "卸载", a.verifyAppAbsent("jellyfin"))
	if !errors.Is(err, ErrTaskOutcomeUnknown) {
		t.Fatalf("err = %v, want ErrTaskOutcomeUnknown", err)
	}
	if !strings.Contains(err.Error(), "仍在已装列表") {
		t.Errorf("err = %q, want the observed presence as evidence", err)
	}
}
