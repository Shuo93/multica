package logger

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/creack/pty"
)

// TestNewWriterLoggerDefault verifies that both the returned component logger
// and bare slog.* calls write to the supplied writer (not os.Stderr), with the
// component tag attached and color escapes disabled. This is the contract the
// daemon relies on to funnel every log line into its rotating daemon.log.
func TestNewWriterLoggerDefault(t *testing.T) {
	// slog.SetDefault mutates global state, so this test cannot run in parallel
	// with others touching the default logger; restore it afterwards.
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var buf bytes.Buffer
	log := NewWriterLoggerDefault("daemon", &buf)

	log.Error("boom", "code", 42)
	slog.Warn("global-line")

	out := buf.String()
	if !strings.Contains(out, "boom") {
		t.Errorf("component logger output missing message: %q", out)
	}
	if !strings.Contains(out, "component=daemon") {
		t.Errorf("output missing component tag: %q", out)
	}
	if !strings.Contains(out, "global-line") {
		t.Errorf("global slog.Warn did not reach the writer: %q", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("output contains ANSI color escapes, want NoColor: %q", out)
	}
}

func TestStderrIsTerminal(t *testing.T) {
	t.Run("null device", func(t *testing.T) {
		f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		checkStderrIsTerminal(t, f, false)
	})

	t.Run("regular file", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "stderr")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		checkStderrIsTerminal(t, f, false)
	})

	t.Run("pipe", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		checkStderrIsTerminal(t, w, false)
	})

	t.Run("pseudo-terminal", func(t *testing.T) {
		master, terminal, err := pty.Open()
		if err == pty.ErrUnsupported {
			t.Skip("pseudo-terminals are unsupported by creack/pty on this platform")
		}
		if err != nil {
			t.Fatal(err)
		}
		defer master.Close()
		defer terminal.Close()
		checkStderrIsTerminal(t, terminal, true)
	})
}

func checkStderrIsTerminal(t *testing.T, f *os.File, want bool) {
	t.Helper()
	previous := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = previous }()

	if got := StderrIsTerminal(); got != want {
		t.Errorf("StderrIsTerminal() = %t, want %t", got, want)
	}
}
