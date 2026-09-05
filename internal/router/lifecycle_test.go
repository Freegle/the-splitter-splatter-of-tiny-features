package router

import (
	"runtime"
	"testing"
	"time"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/store"
)

func TestLiveRouter_StartDoesNotPerformInitialLoad(t *testing.T) {
	db := openTestDB(t)
	lr := NewLiveRouter(db, testUpdateConfig())

	if err := store.UpsertRouterState(db, store.RouterStateRow{
		Category: "single_file_edit|iznik-server-go",
		Families: "claude-sonnet>qwen-coder:7b",
		N:        50, Agreed: 48, WilsonLB: 0.91, Routable: true,
		UpdatedTS: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("UpsertRouterState: %v", err)
	}

	lr.Start()
	defer lr.Stop()

	if _, ok := lr.Lookup("single_file_edit|iznik-server-go", "claude-sonnet>qwen-coder:7b"); ok {
		t.Fatal("Lookup found an entry immediately after Start, but Start should not perform initial load")
	}

	time.Sleep(100 * time.Millisecond)

	if _, ok := lr.Lookup("single_file_edit|iznik-server-go", "claude-sonnet>qwen-coder:7b"); ok {
		t.Fatal("Lookup found an entry after 100ms, but Start should not have refreshed yet (interval is 60s)")
	}

	if err := lr.RefreshSnapshot(); err != nil {
		t.Fatalf("RefreshSnapshot: %v", err)
	}

	entry, ok := lr.Lookup("single_file_edit|iznik-server-go", "claude-sonnet>qwen-coder:7b")
	if !ok {
		t.Fatal("Lookup did not find the entry after explicit RefreshSnapshot")
	}
	if !entry.Routable || entry.N != 50 || entry.Agreed != 48 {
		t.Errorf("entry = %+v, want Routable=true N=50 Agreed=48", entry)
	}
}

func TestLiveRouter_StopEndsRefreshGoroutine(t *testing.T) {
	db := openTestDB(t)
	lr := NewLiveRouter(db, testUpdateConfig())

	baseline := runtime.NumGoroutine()

	lr.Start()

	started := false
	for i := 0; i < 50; i++ {
		if runtime.NumGoroutine() > baseline {
			started = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !started {
		t.Fatal("NumGoroutine did not increase after Start")
	}

	lr.Stop()

	for i := 0; i < 200; i++ {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("NumGoroutine did not return to baseline after Stop")
}

func TestLiveRouter_ShouldShadow_ZeroAndHundredPercent(t *testing.T) {
	tests := []struct {
		name string
		pct  int
		want bool
	}{
		{"zero", 0, false},
		{"hundred", 100, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testUpdateConfig()
			cfg.Router.DualDispatchPct = tt.pct
			lr := NewLiveRouter(openTestDB(t), cfg)

			for i := 0; i < 20; i++ {
				got := lr.ShouldShadow()
				if got != tt.want {
					t.Errorf("iteration %d: ShouldShadow() = %v, want %v", i, got, tt.want)
				}
			}
		})
	}
}

func TestLiveRouter_ShouldShadow_ConsumesAnOrdinalPerCall(t *testing.T) {
	cfg := testUpdateConfig()
	lr := NewLiveRouter(openTestDB(t), cfg)

	for i := 0; i < 5; i++ {
		lr.ShouldShadow()
	}

	if got := lr.NextOrdinal(); got != 5 {
		t.Errorf("NextOrdinal() = %d, want 5 after 5 ShouldShadow calls", got)
	}
}

func TestLiveRouter_ShouldShadow_IsDeterministicAcrossRouters(t *testing.T) {
	cfg := testUpdateConfig()
	cfg.Router.DualDispatchPct = 50

	lr1 := NewLiveRouter(openTestDB(t), cfg)
	lr2 := NewLiveRouter(openTestDB(t), cfg)

	var seq1, seq2 []bool
	for i := 0; i < 200; i++ {
		seq1 = append(seq1, lr1.ShouldShadow())
	}
	for i := 0; i < 200; i++ {
		seq2 = append(seq2, lr2.ShouldShadow())
	}

	if len(seq1) != len(seq2) {
		t.Fatalf("sequence lengths differ: %d vs %d", len(seq1), len(seq2))
	}

	for i := range seq1 {
		if seq1[i] != seq2[i] {
			t.Fatalf("sequences diverge at index %d: %v vs %v", i, seq1[i], seq2[i])
		}
	}

	trueCount := 0
	for _, v := range seq1 {
		if v {
			trueCount++
		}
	}
	if trueCount == 0 || trueCount == 200 {
		t.Errorf("true count = %d, expected a non-degenerate sample (not 0 or 200)", trueCount)
	}
}

func TestLiveRouter_BackendModel(t *testing.T) {
	tests := []struct {
		name          string
		replayBackend string
		backends      map[string]config.BackendConfig
		wantName      string
		wantModel     string
		wantOk        bool
	}{
		{
			name:          "configured backend exists",
			replayBackend: "testbackend",
			backends: map[string]config.BackendConfig{
				"testbackend": {BaseURL: "http://example.com", Model: "test-model"},
			},
			wantName:  "testbackend",
			wantModel: "test-model",
			wantOk:    true,
		},
		{
			name:          "configured backend missing from map",
			replayBackend: "nonexistent",
			backends: map[string]config.BackendConfig{
				"other": {BaseURL: "http://example.com", Model: "other-model"},
			},
			wantName:  "nonexistent",
			wantModel: "",
			wantOk:    false,
		},
		{
			name:          "empty backend name and missing key",
			replayBackend: "",
			backends: map[string]config.BackendConfig{
				"other": {BaseURL: "http://example.com", Model: "other-model"},
			},
			wantName:  "",
			wantModel: "",
			wantOk:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testUpdateConfig()
			cfg.Replay.Backend = tt.replayBackend
			cfg.Backends = tt.backends

			lr := NewLiveRouter(openTestDB(t), cfg)

			name, model, ok := lr.BackendModel()
			if name != tt.wantName {
				t.Errorf("name = %q, want %q", name, tt.wantName)
			}
			if model != tt.wantModel {
				t.Errorf("model = %q, want %q", model, tt.wantModel)
			}
			if ok != tt.wantOk {
				t.Errorf("ok = %v, want %v", ok, tt.wantOk)
			}
		})
	}
}
