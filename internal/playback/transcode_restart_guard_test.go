package playback

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/tonemap"
)

func TestIsHardwareTranscodeRecognizesAutoAndQSV(t *testing.T) {
	for _, hw := range []string{"qsv", "vaapi", "nvenc", "auto", "QSV", "Auto"} {
		if !IsHardwareTranscode(hw) {
			t.Errorf("IsHardwareTranscode(%q) = false, want true", hw)
		}
	}
	for _, hw := range []string{"none", "", "cpu", "soft"} {
		if IsHardwareTranscode(hw) {
			t.Errorf("IsHardwareTranscode(%q) = true, want false", hw)
		}
	}
}

func TestForwardRestartPreservesConfiguredBackBuffer(t *testing.T) {
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("`true` not found in PATH: %v", err)
	}

	dir := t.TempDir()
	old := time.Now().Add(-time.Minute)
	for _, segment := range []int{3, 40, 64, 65, 69, 70, 100} {
		name := filepath.Join(dir, segmentFilename(segment, TranscodeOpts{}))
		if err := os.WriteFile(name, []byte("segment"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(name, old, old); err != nil {
			t.Fatal(err)
		}
	}
	session := &TranscodeSession{
		outputDir:            dir,
		lastRequestedSegment: 70,
		lastCompletedSegment: 70,
		lastPruneFloor:       -1,
		opts: TranscodeOpts{
			OutputDir:               dir,
			TargetCodecVideo:        "h264",
			SegmentDuration:         4,
			SegmentRetentionSeconds: 20,
			StartSegmentNumber:      0,
			FFmpegPath:              truePath,
		},
	}

	if err := session.Restart(context.Background(), 400, 100); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	// Restart itself has no downstream-completion proof for the replacement
	// generation, so it must not unlink even old files synchronously.
	for _, segment := range []int{3, 40, 64, 65, 69, 70, 100} {
		path := filepath.Join(dir, segmentFilename(segment, TranscodeOpts{}))
		if _, err := os.Stat(path); err != nil {
			t.Errorf("segment %d was removed during restart: %v", segment, err)
		}
	}

	// Once the replacement generation has produced a complete back buffer,
	// the preserved range must become eligible again rather than leaking one
	// full window after every forward seek.
	session.ReportSegmentDownloaded(105)
	waitForPrunerFileMissing(t, filepath.Join(dir, segmentFilename(70, TranscodeOpts{})))
	for _, segment := range []int{3, 40, 64, 65, 69, 70} {
		path := filepath.Join(dir, segmentFilename(segment, TranscodeOpts{}))
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expired pre-restart segment %d survived completed-download pruning: %v", segment, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, segmentFilename(100, TranscodeOpts{}))); err != nil {
		t.Fatalf("replacement startup segment was removed: %v", err)
	}
}

func TestNonForwardRestartKeepsPreservedRangePrunable(t *testing.T) {
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("`true` not found in PATH: %v", err)
	}

	dir := t.TempDir()
	old := time.Now().Add(-time.Minute)
	for _, segment := range []int{3, 40, 64, 65, 70} {
		name := filepath.Join(dir, segmentFilename(segment, TranscodeOpts{}))
		if err := os.WriteFile(name, []byte("segment"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(name, old, old); err != nil {
			t.Fatal(err)
		}
	}
	session := &TranscodeSession{
		outputDir:            dir,
		lastRequestedSegment: 70,
		lastCompletedSegment: 70,
		lastPruneFloor:       40,
		opts: TranscodeOpts{
			OutputDir:               dir,
			TargetCodecVideo:        "h264",
			SegmentDuration:         4,
			SegmentRetentionSeconds: 20,
			StartSegmentNumber:      0,
			FFmpegPath:              truePath,
		},
	}

	// Audio switches commonly restart from the reported playback position,
	// which can trail the player's completed-download high-water mark.
	session.SetAudioTrackIndex(1)
	if err := session.Restart(context.Background(), 260, 65); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	session.mu.Lock()
	pruneFloor := session.lastPruneFloor
	pruneBeforeStart := session.pruneBeforeStart
	session.mu.Unlock()
	if pruneFloor != 40 || !pruneBeforeStart {
		t.Fatalf("restart prune state = floor %d, pre-start %t; want floor 40, pre-start true", pruneFloor, pruneBeforeStart)
	}

	session.ReportSegmentDownloaded(70)
	waitForPrunerFileMissing(t, filepath.Join(dir, segmentFilename(64, TranscodeOpts{})))
	for _, segment := range []int{3, 40, 64} {
		path := filepath.Join(dir, segmentFilename(segment, TranscodeOpts{}))
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expired pre-restart segment %d survived completed-download pruning: %v", segment, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, segmentFilename(65, TranscodeOpts{}))); err != nil {
		t.Fatalf("replacement startup segment was removed: %v", err)
	}
}

func TestForwardRestartWithoutCompletedDownloadPreservesFreshFiles(t *testing.T) {
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("`true` not found in PATH: %v", err)
	}

	dir := t.TempDir()
	for _, segment := range []int{3, 40, 70} {
		if err := os.WriteFile(filepath.Join(dir, segmentFilename(segment, TranscodeOpts{})), []byte("segment"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	session := &TranscodeSession{
		outputDir:            dir,
		lastRequestedSegment: 70,
		lastCompletedSegment: -1,
		lastPruneFloor:       -1,
		opts: TranscodeOpts{
			OutputDir:               dir,
			TargetCodecVideo:        "h264",
			SegmentDuration:         4,
			SegmentRetentionSeconds: 20,
			StartSegmentNumber:      0,
			FFmpegPath:              truePath,
		},
	}

	if err := session.Restart(context.Background(), 400, 100); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	for _, segment := range []int{3, 40, 70} {
		if _, err := os.Stat(filepath.Join(dir, segmentFilename(segment, TranscodeOpts{}))); err != nil {
			t.Errorf("fresh segment %d was removed without a completed download: %v", segment, err)
		}
	}
}

func TestForwardRestartKeepsSegmentsWhenRetentionDisabled(t *testing.T) {
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("`true` not found in PATH: %v", err)
	}

	dir := t.TempDir()
	for _, segment := range []int{3, 40, 70} {
		name := filepath.Join(dir, segmentFilename(segment, TranscodeOpts{}))
		if err := os.WriteFile(name, []byte("segment"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	session := &TranscodeSession{
		outputDir:            dir,
		lastRequestedSegment: 70,
		lastCompletedSegment: 70,
		opts: TranscodeOpts{
			OutputDir:          dir,
			TargetCodecVideo:   "h264",
			SegmentDuration:    4,
			StartSegmentNumber: 0,
			FFmpegPath:         truePath,
		},
	}

	if err := session.Restart(context.Background(), 400, 100); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	for _, segment := range []int{3, 40, 70} {
		path := filepath.Join(dir, segmentFilename(segment, TranscodeOpts{}))
		if _, err := os.Stat(path); err != nil {
			t.Errorf("disabled retention removed segment %d: %v", segment, err)
		}
	}
}

// TestSegmentRecoveryDecisionWaitsWhileRestarting covers half of issue #243's
// seek-freeze: while a restart is already in flight, a concurrent segment
// request must WAIT for the restart's output rather than trigger another
// restart. Without this, pipelined HLS segment requests spawn dueling ffmpeg
// restarts that keep preempting the segment the player is blocked on.
func TestSegmentRecoveryDecisionWaitsWhileRestarting(t *testing.T) {
	session := &TranscodeSession{
		outputDir:  t.TempDir(),
		restarting: &restartFlight{done: make(chan struct{})},
		opts: TranscodeOpts{
			TargetCodecVideo:   "h264",
			SegmentDuration:    2,
			StartSegmentNumber: 0,
		},
	}

	decision := session.SegmentRecoveryDecision(10, time.Now())
	if decision.Reason != "transcode_restarting" {
		t.Fatalf("Reason = %q, want transcode_restarting", decision.Reason)
	}
	if !decision.Wait {
		t.Error("Wait = false, want true (concurrent requests must wait out an in-flight restart)")
	}
	if decision.RestartOnTimeout {
		t.Error("RestartOnTimeout = true, want false (a timed-out wait must re-decide, not blindly restart)")
	}
}

// TestRestartInvokesRestartHook verifies that a successful Restart fires the
// session's restart hook. The API handler uses the hook to re-arm the
// throttler and the exit monitor; firing it from Restart itself keeps every
// restart caller of a hook-wired session (web segment recovery, audio
// switch) consistent instead of each call site remembering to re-arm by
// hand.
func TestRestartInvokesRestartHook(t *testing.T) {
	// `true` starts and exits cleanly, standing in for ffmpeg. Resolve it
	// via PATH — it lives in /bin on Linux but /usr/bin on macOS.
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("`true` not found in PATH: %v", err)
	}

	session := &TranscodeSession{
		outputDir: t.TempDir(),
		opts: TranscodeOpts{
			TargetCodecVideo:   "h264",
			SegmentDuration:    2,
			StartSegmentNumber: 0,
			FFmpegPath:         truePath,
		},
	}

	hookFired := make(chan struct{}, 1)
	session.SetRestartHook(func(context.Context) {
		hookFired <- struct{}{}
	})

	if err := session.Restart(context.Background(), 20, 10); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	select {
	case <-hookFired:
	case <-time.After(2 * time.Second):
		t.Fatal("restart hook was not invoked after successful restart")
	}
}

func TestRestartCopySeekOriginIsReplacedOrCleared(t *testing.T) {
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("`true` not found in PATH: %v", err)
	}

	newSession := func() *TranscodeSession {
		return &TranscodeSession{
			outputDir: t.TempDir(),
			opts: TranscodeOpts{
				TargetCodecVideo:       "copy",
				SegmentDuration:        2,
				SeekSeconds:            18,
				StreamOriginSeconds:    10,
				CopySeekAnchorResolved: true,
				StartSegmentNumber:     5,
				FFmpegPath:             truePath,
			},
		}
	}

	resolved := newSession()
	if err := resolved.RestartWithCopySeekAnchor(context.Background(), 100, 48, 96); err != nil {
		t.Fatalf("RestartWithCopySeekAnchor: %v", err)
	}
	resolvedOpts := resolved.Opts()
	if resolvedOpts.SeekSeconds != 100 || resolvedOpts.StreamOriginSeconds != 96 ||
		!resolvedOpts.CopySeekAnchorResolved || resolvedOpts.StartSegmentNumber != 48 {
		t.Fatalf("resolved restart opts = %+v", resolvedOpts)
	}

	unresolved := newSession()
	if err := unresolved.Restart(context.Background(), 100, 50); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	unresolvedOpts := unresolved.Opts()
	if unresolvedOpts.StreamOriginSeconds != 0 || unresolvedOpts.CopySeekAnchorResolved {
		t.Fatalf("generic restart retained stale copy origin: %+v", unresolvedOpts)
	}
}

// TestRestartWaiterReceivesInFlightOutcome covers the single-flight outcome:
// a caller arriving while a restart is in progress must not perform its own
// restart, and it must receive the in-flight restart's result instead of
// assuming success. The first caller here fails validation; the waiter must
// surface that failure rather than returning nil.
func TestRestartWaiterReceivesInFlightOutcome(t *testing.T) {
	session := &TranscodeSession{
		outputDir:  t.TempDir(),
		restarting: &restartFlight{done: make(chan struct{})},
		opts: TranscodeOpts{
			TargetCodecVideo:   "h264",
			SegmentDuration:    2,
			StartSegmentNumber: 0,
			// Nonexistent binary: if the waiter were to start its own restart
			// instead of joining the flight, exec fails and the call returns an
			// error, failing the assertions below.
			FFmpegPath: "/nonexistent/ffmpeg-single-flight-test",
		},
	}
	flight := session.restarting

	// The in-flight leader completes its restart with a validation failure.
	go func() {
		session.mu.Lock()
		flight.err = tonemap.ErrSourceRevisionChanged
		session.mu.Unlock()
		close(flight.done)
	}()

	err := session.Restart(context.Background(), 20, 10)
	if !errors.Is(err, tonemap.ErrSourceRevisionChanged) {
		t.Fatalf("Restart during in-flight restart = %v, want the in-flight validation outcome", err)
	}

	session.mu.Lock()
	restartCount := session.restartCount
	session.mu.Unlock()
	if restartCount != 0 {
		t.Errorf("restartCount = %d, want 0 (waiter must not perform a restart)", restartCount)
	}
}

// TestRestartSeekTarget_CopyForwardJumpRequiresBoundedEnvelope pins the guard
// around the copy-mode forward-jump fallback: it only fabricates a seek target
// when the media duration is known and the requested segment lands inside that
// envelope at or after the generation's first segment. Unknown-duration,
// past-the-end, and before-start targets must stay unresolved so the caller
// keeps its retryable-miss behavior.
func TestRestartSeekTarget_CopyForwardJumpRequiresBoundedEnvelope(t *testing.T) {
	base := func() *TranscodeSession {
		return &TranscodeSession{
			outputDir: t.TempDir(),
			opts: TranscodeOpts{
				SeekSeconds:            18.261,
				StreamOriginSeconds:    18,
				CopySeekAnchorResolved: true,
				TargetCodecVideo:       "copy",
				SegmentDuration:        2,
				StartSegmentNumber:     9,
				TotalDuration:          120,
			},
		}
	}

	unknown := base()
	unknown.opts.TotalDuration = 0
	if got, ok, err := unknown.RestartSeekTarget(40); err != nil || ok || got != 0 {
		t.Fatalf("unknown-duration target = (%v, %v, %v), want (0, false, nil)", got, ok, err)
	}

	beyond := base()
	beyond.opts.TotalDuration = 60
	if got, ok, err := beyond.RestartSeekTarget(40); err != nil || ok || got != 0 {
		t.Fatalf("past-envelope target = (%v, %v, %v), want (0, false, nil)", got, ok, err)
	}

	before := base()
	if got, ok, err := before.RestartSeekTarget(5); err != nil || ok || got != 0 {
		t.Fatalf("before-start target = (%v, %v, %v), want (0, false, nil)", got, ok, err)
	}

	inEnvelope := base()
	got, ok, err := inEnvelope.RestartSeekTarget(40)
	if err != nil || !ok {
		t.Fatalf("bounded target = (%v, %v, %v), want (80, true, nil)", got, ok, err)
	}
	// base 18 + (40-9)*2 = 80.
	if math.Abs(got-80) > 0.0001 {
		t.Fatalf("bounded target = %.6f, want 80", got)
	}
}

// TestRestartSeekTarget_CopyForwardJumpScalesByManifestAverage pins the
// forward-jump estimate against the manifest's real segment timing. hls_time is
// only a lower bound: keyframe-aligned copy fragments run longer, so a long
// jump computed from the nominal duration lands early and serves content ahead
// of the timeline. With five produced fragments averaging 2.5s the estimate
// must use that average; with a single fragment it falls back to nominal 2s.
func TestRestartSeekTarget_CopyForwardJumpScalesByManifestAverage(t *testing.T) {
	baseOpts := TranscodeOpts{
		SeekSeconds:            18.261,
		StreamOriginSeconds:    18,
		CopySeekAnchorResolved: true,
		TargetCodecVideo:       "copy",
		SegmentDuration:        2,
		StartSegmentNumber:     9,
		TotalDuration:          1000,
	}
	writeManifest := func(t *testing.T, dir, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "stream.m3u8"), []byte(body), 0o644); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
	}
	writeProducedSegments := func(t *testing.T, dir string, numbers ...int) {
		t.Helper()
		for _, number := range numbers {
			name := filepath.Join(dir, segmentFilename(number, baseOpts))
			if err := os.WriteFile(name, []byte("segment"), 0o644); err != nil {
				t.Fatalf("write segment %d: %v", number, err)
			}
		}
	}

	fiveDir := t.TempDir()
	fiveSegments := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:3\n#EXT-X-MEDIA-SEQUENCE:9\n#EXT-X-MAP:URI=\"init.mp4\"\n"
	for i := 0; i < 5; i++ {
		fiveSegments += fmt.Sprintf("#EXTINF:2.500000,\nseg_%05d.m4s\n", 9+i)
	}
	writeManifest(t, fiveDir, fiveSegments)
	writeProducedSegments(t, fiveDir, 9, 10, 11, 12, 13)

	averaged := &TranscodeSession{outputDir: fiveDir, opts: baseOpts}
	got, ok, err := averaged.RestartSeekTarget(109)
	if err != nil || !ok {
		t.Fatalf("averaged RestartSeekTarget = (%v, %v, %v), want (268, true, nil)", got, ok, err)
	}
	// base 18 + (109-9)*2.5 = 268; the nominal 2s would give 218.
	if math.Abs(got-268) > 0.0001 {
		t.Fatalf("averaged forward jump = %.6f, want 268 (manifest average 2.5s)", got)
	}

	// A manifest entry with no produced file is not a real timing: with only one
	// produced segment the estimate must fall back to the nominal duration.
	singleDir := t.TempDir()
	writeManifest(t, singleDir, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:3\n#EXT-X-MEDIA-SEQUENCE:9\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:2.500000,\nseg_00009.m4s\n")
	writeProducedSegments(t, singleDir, 9)

	nominal := &TranscodeSession{outputDir: singleDir, opts: baseOpts}
	got, ok, err = nominal.RestartSeekTarget(109)
	if err != nil || !ok {
		t.Fatalf("nominal RestartSeekTarget = (%v, %v, %v), want (218, true, nil)", got, ok, err)
	}
	// base 18 + (109-9)*2 = 218 when fewer than two produced timings exist.
	if math.Abs(got-218) > 0.0001 {
		t.Fatalf("nominal forward jump = %.6f, want 218 (nominal 2s)", got)
	}
}
