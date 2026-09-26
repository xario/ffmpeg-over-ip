package ondemand

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steelbrain/ffmpeg-over-ip/internal/protocol"
)

func TestRequiresGPU(t *testing.T) {
	mgr := &Manager{}

	tests := []struct {
		name     string
		program  byte
		args     []string
		expected bool
	}{
		{
			name:     "ffprobe is always false",
			program:  protocol.ProgramFFprobe,
			args:     []string{"-v", "error", "-show_format", "video.mp4"},
			expected: false,
		},
		{
			name:     "empty args",
			program:  protocol.ProgramFFmpeg,
			args:     nil,
			expected: false,
		},
		{
			name:     "ffmpeg -version",
			program:  protocol.ProgramFFmpeg,
			args:     []string{"-version"},
			expected: false,
		},
		{
			name:     "ffmpeg --version",
			program:  protocol.ProgramFFmpeg,
			args:     []string{"--version"},
			expected: false,
		},
		{
			name:     "ffmpeg -hwaccels probe",
			program:  protocol.ProgramFFmpeg,
			args:     []string{"-hwaccels"},
			expected: false,
		},
		{
			name:     "ffmpeg -decoders probe",
			program:  protocol.ProgramFFmpeg,
			args:     []string{"-decoders"},
			expected: false,
		},
		{
			name:     "ffmpeg -encoders probe",
			program:  protocol.ProgramFFmpeg,
			args:     []string{"-encoders"},
			expected: false,
		},
		{
			name:     "ffmpeg -filters probe",
			program:  protocol.ProgramFFmpeg,
			args:     []string{"-filters"},
			expected: false,
		},
		{
			name:     "ffmpeg -buildconf probe",
			program:  protocol.ProgramFFmpeg,
			args:     []string{"-buildconf"},
			expected: false,
		},
		{
			name:     "ffmpeg -protocols probe",
			program:  protocol.ProgramFFmpeg,
			args:     []string{"-protocols"},
			expected: false,
		},
		{
			name:     "ffmpeg stream copy without hwaccel",
			program:  protocol.ProgramFFmpeg,
			args:     []string{"-i", "input.mp4", "-c:v", "copy", "-c:a", "copy", "output.mp4"},
			expected: false,
		},
		{
			name:     "ffmpeg software cpu encode",
			program:  protocol.ProgramFFmpeg,
			args:     []string{"-i", "input.mkv", "-c:v", "libx264", "-crf", "23", "output.mp4"},
			expected: false,
		},
		{
			name:     "ffmpeg vaapi hwaccel encode",
			program:  protocol.ProgramFFmpeg,
			args:     []string{"-hwaccel", "vaapi", "-i", "input.mkv", "-c:v", "h264_vaapi", "output.mp4"},
			expected: true,
		},
		{
			name:     "ffmpeg qsv hwaccel encode",
			program:  protocol.ProgramFFmpeg,
			args:     []string{"-hwaccel", "qsv", "-i", "input.mkv", "-c:v", "h264_qsv", "output.mp4"},
			expected: true,
		},
		{
			name:     "ffmpeg vaapi device option",
			program:  protocol.ProgramFFmpeg,
			args:     []string{"-vaapi_device", "/dev/dri/renderD128", "-i", "input.mkv", "output.mp4"},
			expected: true,
		},
		{
			name:     "ffmpeg init_hw_device option",
			program:  protocol.ProgramFFmpeg,
			args:     []string{"-init_hw_device", "vaapi=va:/dev/dri/renderD128", "-i", "input.mkv", "output.mp4"},
			expected: true,
		},
		{
			name:     "ffmpeg nvenc encode",
			program:  protocol.ProgramFFmpeg,
			args:     []string{"-i", "input.mkv", "-c:v", "h264_nvenc", "output.mp4"},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mgr.RequiresGPU(tt.program, tt.args)
			if got != tt.expected {
				t.Errorf("RequiresGPU(%v, %v) = %v; want %v", tt.program, tt.args, got, tt.expected)
			}
		})
	}
}

func TestOnDemandLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	startScript := filepath.Join(tmpDir, "start.sh")
	stopScript := filepath.Join(tmpDir, "stop.sh")
	stateFile := filepath.Join(tmpDir, "state")

	startContent := "#!/bin/sh\necho awake > " + stateFile + "\n"
	stopContent := "#!/bin/sh\necho asleep > " + stateFile + "\n"

	if err := os.WriteFile(startScript, []byte(startContent), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stopScript, []byte(stopContent), 0755); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(startScript, stopScript, "100ms")
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	mgr.devicePath = filepath.Join(tmpDir, "fake-render")
	mgr.gpuActive = false // start test with GPU sleeping

	// Initial acquire should run start script
	if err := mgr.Acquire(); err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	content, err := os.ReadFile(stateFile)
	if err != nil || strings.TrimSpace(string(content)) != "awake" {
		t.Fatalf("expected state 'awake', got %q (err: %v)", string(content), err)
	}

	// Release session, starting 100ms idle timer
	mgr.Release()

	// Wait 250ms for idle timer to fire
	time.Sleep(250 * time.Millisecond)

	content, err = os.ReadFile(stateFile)
	if err != nil || strings.TrimSpace(string(content)) != "asleep" {
		t.Fatalf("expected state 'asleep' after idle timeout, got %q (err: %v)", string(content), err)
	}
}
