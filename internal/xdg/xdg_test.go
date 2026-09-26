package xdg

import (
	"path/filepath"
	"testing"
)

func TestDataHome_HonorsXDGDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/tmp/xdgdata")
	got, err := DataHome()
	if err != nil {
		t.Fatalf("DataHome: %v", err)
	}
	if got != "/tmp/xdgdata" {
		t.Errorf("DataHome() = %q, want %q", got, "/tmp/xdgdata")
	}
}

func TestDataHome_FallsBackToHomeLocalShare(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/tmp/fakehome")
	got, err := DataHome()
	if err != nil {
		t.Fatalf("DataHome: %v", err)
	}
	want := filepath.Join("/tmp/fakehome", ".local", "share")
	if got != want {
		t.Errorf("DataHome() = %q, want %q", got, want)
	}
}
