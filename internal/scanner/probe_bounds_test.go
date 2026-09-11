package scanner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProbeFileRejectsOversizedOutput(t *testing.T) {
	ffprobe := filepath.Join(t.TempDir(), "ffprobe")
	script := "#!/bin/sh\ndd if=/dev/zero bs=1048576 count=9 2>/dev/null\n"
	if err := os.WriteFile(ffprobe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := ProbeFile(context.Background(), ffprobe, "provider-controlled")
	if !errors.Is(err, errFFprobeOutputTooLarge) {
		t.Fatalf("ProbeFile error = %v, want output-size rejection", err)
	}
}

func TestBoundedProbeBufferRejectsOverflow(t *testing.T) {
	buffer := &boundedProbeBuffer{limit: 4}
	if _, err := buffer.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	if _, err := buffer.Write([]byte("5")); !errors.Is(err, errFFprobeOutputTooLarge) {
		t.Fatalf("overflow error = %v", err)
	}
}

func TestBuildProbeArgsBoundsRemoteAnalysis(t *testing.T) {
	const input = "virtual://movie/tt1"
	args := buildProbeArgs(input)

	for flag, want := range map[string]string{
		"-probesize":       "32M",
		"-analyzeduration": "10M",
	} {
		got, ok := probeArgValue(args, flag)
		if !ok {
			t.Fatalf("buildProbeArgs missing %s: %v", flag, args)
		}
		if got != want {
			t.Fatalf("%s = %q, want %q", flag, got, want)
		}
	}
	if args[len(args)-1] != input {
		t.Fatalf("input path must be last so ffprobe treats it as the input: %v", args)
	}
}

func probeArgValue(args []string, flag string) (string, bool) {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}
