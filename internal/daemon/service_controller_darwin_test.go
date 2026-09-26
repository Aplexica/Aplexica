//go:build darwin

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// leakedLaunchctlCalls records attempts to reach the real launchctl from
// inside the test binary. It must stay empty: see TestMain.
var leakedLaunchctlCalls struct {
	mu    sync.Mutex
	calls [][]string
}

// TestMain replaces execLaunchctl for the whole test binary. launchctl
// resolves gui/<uid>/com.aplexica.aplexicad whatever HOME a test sets, so a
// test that reached the real launchctl would boot out or restart the daemon
// of the machine running the suite. A test that exercises the controller
// installs a fakeLaunchd instead.
func TestMain(m *testing.M) {
	execLaunchctl = func(args ...string) ([]byte, error) {
		leakedLaunchctlCalls.mu.Lock()
		defer leakedLaunchctlCalls.mu.Unlock()
		leakedLaunchctlCalls.calls = append(leakedLaunchctlCalls.calls, append([]string(nil), args...))
		return nil, fmt.Errorf("test attempted to run the real `launchctl %s`", strings.Join(args, " "))
	}
	code := m.Run()
	leakedLaunchctlCalls.mu.Lock()
	leaked := leakedLaunchctlCalls.calls
	leakedLaunchctlCalls.mu.Unlock()
	if len(leaked) > 0 {
		fmt.Fprintf(os.Stderr,
			"\nFAIL: %d launchctl invocation(s) escaped the test hook and would have reached\n"+
				"the real gui/$UID/%s on this machine. Use useFakeLaunchd instead:\n  %v\n",
			len(leaked), launchdLabel, leaked)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// fakeLaunchd stands in for launchctl. It models the one piece of launchd
// state the controller depends on, whether the job is registered in the GUI
// domain, including the stretch after bootout during which the job is still
// registered because its process has not exited. Its replies follow what
// launchctl does on macOS in each case: bootstrap of a registered job fails
// with a generic I/O error, bootout of an unregistered one with "No such
// process", and print and kickstart of an unregistered one with "Could not
// find service". During a teardown bootstrap fails too, while kickstart
// succeeds without starting anything.
type fakeLaunchd struct {
	loaded bool
	// teardownProbes is how many `print` probes after a bootout still find
	// the job registered; tearingDown counts them down.
	teardownProbes int
	tearingDown    int
	// brokenPlist makes bootstrap fail without loading anything, the way an
	// unreadable or malformed plist does.
	brokenPlist bool
	// refuseBootout makes bootout fail while the job stays loaded.
	refuseBootout bool
	calls         [][]string
}

func (f *fakeLaunchd) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	notFound := []byte(`Could not find service "com.aplexica.aplexicad" in domain for user gui: 501`)
	switch args[0] {
	case "print":
		if f.tearingDown > 0 {
			f.tearingDown--
			return []byte("state = SIGTERMed"), nil
		}
		if f.loaded {
			return []byte("state = running"), nil
		}
		return notFound, errors.New("exit status 113")
	case "bootstrap":
		if f.loaded || f.tearingDown > 0 || f.brokenPlist {
			return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
		}
		f.loaded = true
		return nil, nil
	case "bootout":
		if f.refuseBootout {
			return []byte("Boot-out failed: 1: Operation not permitted"), errors.New("exit status 1")
		}
		if !f.loaded {
			return []byte("Boot-out failed: 3: No such process"), errors.New("exit status 3")
		}
		f.loaded = false
		f.tearingDown = f.teardownProbes
		return nil, nil
	case "kickstart":
		if f.tearingDown > 0 {
			return nil, nil
		}
		if !f.loaded {
			return notFound, errors.New("exit status 113")
		}
		return nil, nil
	}
	return nil, fmt.Errorf("fakeLaunchd: unexpected launchctl %v", args)
}

// useFakeLaunchd routes the controller's launchctl calls to f for the
// duration of one test, restoring TestMain's poison afterwards.
func useFakeLaunchd(t *testing.T, f *fakeLaunchd) {
	t.Helper()
	prev := execLaunchctl
	execLaunchctl = f.run
	t.Cleanup(func() { execLaunchctl = prev })
}

// launchdTestTargets points HOME at a temp dir and returns the domain, job
// target and plist path the controller must hand to launchctl.
func launchdTestTargets(t *testing.T) (domain, target, plist string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	domain = "gui/" + strconv.Itoa(os.Getuid())
	return domain, domain + "/" + launchdLabel,
		filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func requireLaunchctlCalls(t *testing.T, f *fakeLaunchd, want [][]string) {
	t.Helper()
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("launchctl calls:\n got  %v\n want %v", f.calls, want)
	}
}

func TestLaunchdControllerManagedTracksThePlist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	ctl := ServiceControllerForPlatform()
	if ctl == nil {
		t.Fatal("darwin must ship a service controller")
	}
	if ctl.Managed() {
		t.Fatal("Managed must be false when no LaunchAgent is installed")
	}

	agents := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(agents, launchdLabel+".plist")
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ctl.Managed() {
		t.Fatalf("Managed must be true once %s exists", plist)
	}
}

func TestLaunchdControllerLabelMatchesTheInstaller(t *testing.T) {
	ctl := ServiceControllerForPlatform()
	installer := &launchdInstaller{}
	if ctl.Label() != installer.PlatformLabel() {
		t.Fatalf("controller label %q must match installer label %q", ctl.Label(), installer.PlatformLabel())
	}
}

// After `daemon stop` the job is not loaded at all. Start must load it from
// the plist, which starts it (RunAtLoad), and then kickstart it.
func TestLaunchdControllerStartLoadsTheAgentAndKickstartsIt(t *testing.T) {
	domain, target, plist := launchdTestTargets(t)
	fake := &fakeLaunchd{}
	useFakeLaunchd(t, fake)

	if err := ServiceControllerForPlatform().Start(); err != nil {
		t.Fatal(err)
	}
	requireLaunchctlCalls(t, fake, [][]string{
		{"bootstrap", domain, plist},
		{"kickstart", target},
	})
	if !fake.loaded {
		t.Fatal("Start must leave the job loaded")
	}
}

// A job that is loaded but not answering (still starting, or waiting out a
// relaunch) makes bootstrap fail. That failure must be recognised through
// print rather than by its text, and the kickstart must still happen.
func TestLaunchdControllerStartToleratesAnAlreadyLoadedJob(t *testing.T) {
	domain, target, plist := launchdTestTargets(t)
	fake := &fakeLaunchd{loaded: true}
	useFakeLaunchd(t, fake)

	if err := ServiceControllerForPlatform().Start(); err != nil {
		t.Fatal(err)
	}
	requireLaunchctlCalls(t, fake, [][]string{
		{"bootstrap", domain, plist},
		{"print", target},
		{"kickstart", target},
	})
}

// A bootstrap that fails and leaves nothing loaded is a real failure, and
// launchctl's output is the only explanation the user gets.
func TestLaunchdControllerStartReportsABootstrapThatLoadsNothing(t *testing.T) {
	domain, target, plist := launchdTestTargets(t)
	fake := &fakeLaunchd{brokenPlist: true}
	useFakeLaunchd(t, fake)

	err := ServiceControllerForPlatform().Start()
	if err == nil {
		t.Fatal("Start must fail when bootstrap loads nothing")
	}
	for _, want := range []string{"launchctl bootstrap " + domain + " " + plist, "Input/output error"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	requireLaunchctlCalls(t, fake, [][]string{
		{"bootstrap", domain, plist},
		{"print", target},
	})
}

// The plist sets KeepAlive, so the only stop that holds is taking the job
// out of the domain. bootout returns before the process has exited, so Stop
// keeps probing until launchd has removed the job.
func TestLaunchdControllerStopBootsOutAndWaitsForTheJobToGo(t *testing.T) {
	_, target, _ := launchdTestTargets(t)
	fake := &fakeLaunchd{loaded: true, teardownProbes: 2}
	useFakeLaunchd(t, fake)

	if err := ServiceControllerForPlatform().Stop(); err != nil {
		t.Fatal(err)
	}
	requireLaunchctlCalls(t, fake, [][]string{
		{"bootout", target},
		{"print", target},
		{"print", target},
		{"print", target},
	})
	if fake.loaded {
		t.Fatal("Stop must leave the job unloaded")
	}
}

// Stopping a job that is not loaded has nothing to do and must succeed, so
// `daemon stop` stays safe to repeat.
func TestLaunchdControllerStopIsANoOpWhenNothingIsLoaded(t *testing.T) {
	_, target, _ := launchdTestTargets(t)
	fake := &fakeLaunchd{}
	useFakeLaunchd(t, fake)

	if err := ServiceControllerForPlatform().Stop(); err != nil {
		t.Fatalf("Stop with nothing loaded must succeed: %v", err)
	}
	requireLaunchctlCalls(t, fake, [][]string{
		{"bootout", target},
		{"print", target},
	})
}

func TestLaunchdControllerStopReportsARefusedBootout(t *testing.T) {
	_, target, _ := launchdTestTargets(t)
	fake := &fakeLaunchd{loaded: true, refuseBootout: true}
	useFakeLaunchd(t, fake)

	err := ServiceControllerForPlatform().Stop()
	if err == nil {
		t.Fatal("Stop must fail when bootout fails and the job stays loaded")
	}
	if !strings.Contains(err.Error(), "launchctl bootout "+target) {
		t.Errorf("error %q does not name the bootout", err)
	}
}

func TestLaunchdControllerStopGivesUpOnAJobThatNeverLeaves(t *testing.T) {
	_, target, _ := launchdTestTargets(t)
	fake := &fakeLaunchd{loaded: true, teardownProbes: 1 << 20}
	useFakeLaunchd(t, fake)

	err := launchdController{}.stop(50 * time.Millisecond)
	if err == nil {
		t.Fatal("stop must fail when the job is still loaded after the wait")
	}
	if !strings.Contains(err.Error(), "still has "+target+" loaded") {
		t.Errorf("error %q does not say the job is still loaded", err)
	}
}

// `daemon stop` followed straight away by `daemon start` must leave the
// agent loaded. A Start issued while the booted-out job is still being torn
// down is lost: bootstrap fails and kickstart succeeds without starting
// anything. Stop waiting for the teardown is what makes the pair work.
func TestLaunchdControllerStopThenStartLeavesTheAgentLoaded(t *testing.T) {
	launchdTestTargets(t)
	fake := &fakeLaunchd{loaded: true, teardownProbes: 3}
	useFakeLaunchd(t, fake)
	ctl := ServiceControllerForPlatform()

	if err := ctl.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := ctl.Start(); err != nil {
		t.Fatal(err)
	}
	if !fake.loaded {
		t.Fatalf("the agent must be loaded after stop then start; launchctl calls: %v", fake.calls)
	}
}

func TestLaunchdControllerRestartKickstartsALoadedJob(t *testing.T) {
	_, target, _ := launchdTestTargets(t)
	fake := &fakeLaunchd{loaded: true}
	useFakeLaunchd(t, fake)

	if err := ServiceControllerForPlatform().Restart(); err != nil {
		t.Fatal(err)
	}
	requireLaunchctlCalls(t, fake, [][]string{
		{"print", target},
		{"kickstart", "-k", target},
	})
}

// kickstart cannot restart a job that `daemon stop` booted out; Restart must
// load it instead of failing on "Could not find service".
func TestLaunchdControllerRestartAfterStopLoadsTheAgent(t *testing.T) {
	domain, target, plist := launchdTestTargets(t)
	fake := &fakeLaunchd{}
	useFakeLaunchd(t, fake)

	if err := ServiceControllerForPlatform().Restart(); err != nil {
		t.Fatal(err)
	}
	requireLaunchctlCalls(t, fake, [][]string{
		{"print", target},
		{"bootstrap", domain, plist},
		{"kickstart", target},
	})
	if !fake.loaded {
		t.Fatal("Restart must leave the job loaded")
	}
}
