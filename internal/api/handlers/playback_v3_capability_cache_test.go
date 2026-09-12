package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/tonemap"
)

// TestHLSToneMapCapabilityInventoryV3CachesLocalProbeAcrossStarts proves a
// second planning inventory does not re-run the local FFmpeg probe. Planning
// used to pay for it on every start; the inventory is now process-lifetime,
// mirroring transformationRegistryV3.
func TestHLSToneMapCapabilityInventoryV3CachesLocalProbeAcrossStarts(t *testing.T) {
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	var probes atomic.Int32
	handler.v3ToneMapProbe = func(context.Context, string, string, string) (tonemap.Capabilities, error) {
		probes.Add(1)
		return tonemap.Capabilities{{
			Mode: tonemap.ModeSoftware, Backend: tonemap.BackendSoftware,
			Filter: tonemap.SoftwareFilterBT2390, SourceKinds: []tonemap.SourceKind{tonemap.SourcePQ},
		}}, nil
	}

	for i := 0; i < 2; i++ {
		inventory, err := handler.hlsToneMapCapabilityInventoryV3(context.Background())
		if err != nil {
			t.Fatalf("inventory %d: %v", i, err)
		}
		if !inventory.union.Supports(tonemap.ModeSoftware, tonemap.SourcePQ) {
			t.Fatalf("inventory %d = %#v, want the cached local capability", i, inventory)
		}
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("local probe calls = %d, want 1 for two planning inventories", got)
	}
}

// TestLocalToneMapCapabilitiesCachedV3RetriesAfterFailure pins the other half of
// the cache contract: only a successful probe is reused. A transient failure
// has to stay retryable or a busy encoder at startup would freeze the server at
// software-only for the process lifetime.
func TestLocalToneMapCapabilitiesCachedV3RetriesAfterFailure(t *testing.T) {
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	want := tonemap.Capabilities{{
		Mode: tonemap.ModeSoftware, Backend: tonemap.BackendSoftware,
		Filter: tonemap.SoftwareFilterBT2390, SourceKinds: []tonemap.SourceKind{tonemap.SourcePQ},
	}}
	var calls atomic.Int32
	handler.v3ToneMapProbe = func(context.Context, string, string, string) (tonemap.Capabilities, error) {
		if calls.Add(1) == 1 {
			return nil, context.DeadlineExceeded
		}
		return want, nil
	}

	if _, err := handler.localToneMapCapabilitiesCachedV3(context.Background()); err == nil {
		t.Fatal("first probe error was not surfaced")
	}
	got, err := handler.localToneMapCapabilitiesCachedV3(context.Background())
	if err != nil || !got.Supports(tonemap.ModeSoftware, tonemap.SourcePQ) {
		t.Fatalf("second probe = %#v, %v; want the retried successful inventory", got, err)
	}
	if _, err := handler.localToneMapCapabilitiesCachedV3(context.Background()); err != nil {
		t.Fatalf("cached probe: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("probe calls = %d, want 2 (failure retried, success cached)", got)
	}
}

func TestNodeCapabilityExpiringV3(t *testing.T) {
	now := time.Now()
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	const nodeURL = "http://node.example"

	if !handler.nodeCapabilityExpiringV3(nodeURL, now) {
		t.Fatal("missing entry must be due for refresh")
	}
	handler.v3NodeCapabilities = map[string]v3NodeCapabilityCache{
		nodeURL: {err: errors.New("unreachable"), expiresAt: now.Add(time.Minute)},
	}
	if !handler.nodeCapabilityExpiringV3(nodeURL, now) {
		t.Fatal("failed entry must be due for refresh")
	}
	handler.v3NodeCapabilities[nodeURL] = v3NodeCapabilityCache{expiresAt: now.Add(v3NodeCapabilityTTL)}
	if handler.nodeCapabilityExpiringV3(nodeURL, now) {
		t.Fatal("comfortably fresh entry must not be due for refresh")
	}
	handler.v3NodeCapabilities[nodeURL] = v3NodeCapabilityCache{expiresAt: now.Add(v3NodeCapabilityRefreshInterval / 2)}
	if !handler.nodeCapabilityExpiringV3(nodeURL, now) {
		t.Fatal("entry inside the refresh margin must be due for refresh")
	}
}

// TestRefreshStaleNodeCapabilitiesV3RefreshesOnlyDueNodes drives one sweep of
// the background refresher: a node with no cached entry is re-probed, a node
// with a fresh one is left untouched so the refresher does not multiply probe
// traffic.
func TestRefreshStaleNodeCapabilitiesV3RefreshesOnlyDueNodes(t *testing.T) {
	var staleHits, freshHits atomic.Int32
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		staleHits.Add(1)
		writeJSON(w, http.StatusOK, playback.HWAccelInfo{})
	}))
	defer stale.Close()
	fresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		freshHits.Add(1)
		writeJSON(w, http.StatusOK, playback.HWAccelInfo{})
	}))
	defer fresh.Close()

	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.JWTSecret = "test-secret"
	handler.NodePlanner = enumeratingNodePlannerV3{urls: []string{stale.URL, fresh.URL}}
	handler.v3NodeCapabilities = map[string]v3NodeCapabilityCache{
		fresh.URL: {expiresAt: time.Now().Add(v3NodeCapabilityTTL)},
	}

	handler.refreshStaleNodeCapabilitiesV3()

	deadline := time.Now().Add(5 * time.Second)
	for staleHits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := staleHits.Load(); got != 1 {
		t.Fatalf("stale node probes = %d, want 1", got)
	}
	if got := freshHits.Load(); got != 0 {
		t.Fatalf("fresh node probes = %d, want 0", got)
	}
}
