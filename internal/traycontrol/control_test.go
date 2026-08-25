package traycontrol

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestServerReportsIdentityAndQuitsForUpdate(t *testing.T) {
	socket := testSocketPath(t)
	quit := make(chan struct{}, 1)
	server := NewServer(socket, "v1.2.3", func() { quit <- struct{}{} })
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	response, err := Send(context.Background(), socket, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.Version != "v1.2.3" || response.PID != os.Getpid() {
		t.Fatalf("unexpected status response: %+v", response)
	}
	response, err = Send(context.Background(), socket, "quit-for-update")
	if err != nil || !response.OK {
		t.Fatalf("quit response=%+v err=%v", response, err)
	}
	select {
	case <-quit:
	case <-time.After(time.Second):
		t.Fatal("quit callback was not invoked")
	}
}

func TestServerRejectsUnknownCommand(t *testing.T) {
	socket := testSocketPath(t)
	server := NewServer(socket, "v1.2.3", nil)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	response, err := Send(context.Background(), socket, "replace-runtime")
	if err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error == "" {
		t.Fatalf("unexpected unknown-command response: %+v", response)
	}
}

func testSocketPath(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return filepath.Join(t.TempDir(), "tray.sock")
	}
	directory, err := os.MkdirTemp("/tmp", "atc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return filepath.Join(directory, "tray.sock")
}
