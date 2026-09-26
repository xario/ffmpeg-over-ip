package ondemand

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/steelbrain/ffmpeg-over-ip/internal/protocol"
)

type Manager struct {
	mu          sync.Mutex
	startScript string
	stopScript  string
	idleTimeout time.Duration
	activeCount int
	gpuActive   bool
	idleTimer   *time.Timer
	devicePath  string
}

// NewManager creates an on-demand GPU manager.
// If both startScript and stopScript are empty, it returns nil (disabled).
func NewManager(startScript, stopScript, idleTimeoutStr string) (*Manager, error) {
	if startScript == "" && stopScript == "" {
		return nil, nil
	}

	idleTimeout := 1 * time.Minute
	if idleTimeoutStr != "" {
		d, err := time.ParseDuration(idleTimeoutStr)
		if err != nil {
			return nil, fmt.Errorf("invalid idleTimeout %q: %w", idleTimeoutStr, err)
		}
		if d < 10*time.Millisecond {
			return nil, fmt.Errorf("idleTimeout must be at least 10ms, got %v", d)
		}
		idleTimeout = d
	}

	devicePath := "/dev/dri/renderD128"

	m := &Manager{
		startScript: startScript,
		stopScript:  stopScript,
		idleTimeout: idleTimeout,
		devicePath:  devicePath,
	}

	// Check if GPU is currently active
	if m.isGPUPresent() {
		m.gpuActive = true
		log.Printf("[ondemand] initialized with GPU active (startScript: %s, stopScript: %s, idleTimeout: %v)",
			startScript, stopScript, idleTimeout)
		if m.stopScript != "" && m.idleTimeout > 0 {
			log.Printf("[ondemand] scheduled initial idle timer for %v", m.idleTimeout)
			m.idleTimer = time.AfterFunc(m.idleTimeout, m.onIdleTimeout)
		}
	} else {
		m.gpuActive = false
		log.Printf("[ondemand] initialized with GPU sleeping (startScript: %s, stopScript: %s, idleTimeout: %v)",
			startScript, stopScript, idleTimeout)
	}

	return m, nil
}

// isGPUPresent checks if the GPU render node exists
func (m *Manager) isGPUPresent() bool {
	if m.devicePath == "" {
		return true
	}
	_, err := os.Stat(m.devicePath)
	return err == nil
}

// RequiresGPU inspects the program and arguments to decide whether the GPU is needed.
// Informational queries (-version, -hwaccels, etc.) and pure CPU tasks return false,
// ensuring the discrete GPU is never spun up unnecessarily.
func (m *Manager) RequiresGPU(program byte, args []string) bool {
	if program == protocol.ProgramFFprobe {
		return false
	}
	if len(args) == 0 {
		return false
	}

	// Pure probe commands never need GPU hardware
	if isProbeCommand(args) {
		return false
	}

	// Inspect flags and parameters for hardware acceleration
	for i, arg := range args {
		lower := strings.ToLower(arg)

		// Explicit hardware acceleration flags
		switch lower {
		case "-hwaccel", "-hwaccel_device", "-hwaccel_output_format",
			"-vaapi_device", "-qsv_device", "-init_hw_device", "-filter_hw_device":
			return true
		}

		// Device references
		if strings.HasPrefix(lower, "/dev/dri") {
			return true
		}

		// Hardware codec or filter specifications
		if strings.Contains(lower, "qsv") ||
			strings.Contains(lower, "vaapi") ||
			strings.Contains(lower, "nvenc") ||
			strings.Contains(lower, "cuda") ||
			strings.Contains(lower, "cuvid") ||
			strings.Contains(lower, "amf") {
			// Ensure it's not just a flag like -hwaccels handled earlier
			return true
		}

		// Check if argument is an option value attached via '=' (e.g. -init_hw_device=vaapi:...)
		if strings.Contains(lower, "=") {
			parts := strings.SplitN(lower, "=", 2)
			key := parts[0]
			val := parts[1]
			if strings.Contains(key, "hw") || strings.Contains(key, "vaapi") || strings.Contains(key, "qsv") {
				return true
			}
			if strings.Contains(val, "qsv") || strings.Contains(val, "vaapi") || strings.Contains(val, "renderd") {
				return true
			}
		}

		_ = i
	}

	return false
}

// isProbeCommand returns true if the command is purely requesting static capability or version info
func isProbeCommand(args []string) bool {
	probeFlags := map[string]bool{
		"-version":     true,
		"--version":    true,
		"-buildconf":   true,
		"-decoders":    true,
		"-encoders":    true,
		"-filters":     true,
		"-hwaccels":    true,
		"-protocols":   true,
		"-formats":     true,
		"-codecs":      true,
		"-bsfs":        true,
		"-devices":     true,
		"-pix_fmts":    true,
		"-layouts":     true,
		"-sample_fmts": true,
		"-colors":      true,
		"-h":           true,
		"--help":       true,
		"-?":           true,
	}

	hasInput := false
	hasProbe := false
	for _, arg := range args {
		lower := strings.ToLower(arg)
		if lower == "-i" {
			hasInput = true
		}
		if probeFlags[lower] {
			hasProbe = true
		}
	}

	// If it asks for probe info and specifies no input file, it is definitely a probe command
	if hasProbe && !hasInput {
		return true
	}

	return false
}

// Acquire is called when a session that requires the GPU starts.
func (m *Manager) Acquire() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Cancel idle shutdown timer if running
	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
		log.Printf("[ondemand] new session acquired, cancelled idle shutdown timer")
	}

	// Wake GPU if sleeping
	if !m.gpuActive {
		if m.startScript != "" {
			log.Printf("[ondemand] GPU is asleep, running startScript: %s", m.startScript)
			start := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			cmd := exec.CommandContext(ctx, m.startScript)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("startScript failed: %w (output: %s)", err, string(out))
			}
			log.Printf("[ondemand] startScript completed in %v: %s", time.Since(start), strings.TrimSpace(string(out)))
		}
		m.gpuActive = true

		if !m.isGPUPresent() {
			log.Printf("[ondemand] warning: device %s not detected after startScript", m.devicePath)
		}
	}

	m.activeCount++
	log.Printf("[ondemand] session started (active sessions: %d)", m.activeCount)
	return nil
}

// Release is called when a session finishes.
func (m *Manager) Release() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.activeCount--
	if m.activeCount < 0 {
		m.activeCount = 0
	}
	log.Printf("[ondemand] session finished (active sessions: %d)", m.activeCount)

	if m.activeCount == 0 && m.gpuActive && m.stopScript != "" {
		log.Printf("[ondemand] all sessions finished; starting %v idle shutdown timer", m.idleTimeout)
		if m.idleTimer != nil {
			m.idleTimer.Stop()
		}
		m.idleTimer = time.AfterFunc(m.idleTimeout, m.onIdleTimeout)
	}
}

// onIdleTimeout is called when the idle timer expires.
func (m *Manager) onIdleTimeout() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.activeCount > 0 || !m.gpuActive || m.stopScript == "" {
		return
	}

	log.Printf("[ondemand] idle timeout expired (%v); stopping GPU via %s...", m.idleTimeout, m.stopScript)
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, m.stopScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[ondemand] stopScript failed: %v (output: %s)", err, string(out))
	} else {
		log.Printf("[ondemand] GPU stopped successfully in %v, entered low-power mode (output: %s)",
			time.Since(start), strings.TrimSpace(string(out)))
	}
	m.gpuActive = false
	m.idleTimer = nil
}

// Shutdown cleanly stops the manager when the server exits.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}

	if m.gpuActive && m.stopScript != "" {
		log.Printf("[ondemand] server shutting down; running stopScript: %s", m.stopScript)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, m.stopScript)
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("[ondemand] shutdown stopScript failed: %v (output: %s)", err, string(out))
		}
		m.gpuActive = false
	}
}
