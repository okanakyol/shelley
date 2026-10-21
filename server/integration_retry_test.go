package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"shelley.exe.dev/models"
)

// refreshingLLMManager records catalog refreshes the server applies.
type refreshingLLMManager struct {
	testLLMManager
	mu      sync.Mutex
	applied int
}

func (m *refreshingLLMManager) RefreshBuiltModels([]models.Built) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applied++
	return nil
}

func TestRetryUnloadedIntegrations(t *testing.T) {
	svr, _ := newCacheKeyTestServerWithDB(t, "")
	manager := &refreshingLLMManager{}
	svr.llmManager = manager

	// The proxy comes up on the third refresh.
	var mu sync.Mutex
	unloaded, refreshes := []string{"edge"}, 0
	svr.SetModelRefresher(func(context.Context) ([]models.Built, error) {
		mu.Lock()
		defer mu.Unlock()
		if refreshes++; refreshes == 3 {
			unloaded = nil
		}
		return nil, nil
	})
	current := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return unloaded
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		svr.retryUnloadedIntegrations(ctx, current, time.Millisecond, 4*time.Millisecond)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for len(current()) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(current()) > 0 {
		t.Fatal("integration never loaded")
	}
	// Once loaded, the loop keeps watching but stops refreshing.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if refreshes != 3 {
		t.Errorf("refreshes = %d, want 3 (none after the integration loaded)", refreshes)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.applied != 3 {
		t.Errorf("applied refreshes = %d, want 3", manager.applied)
	}
}

func TestRetryUnloadedIntegrationsStopsWithContext(t *testing.T) {
	svr, _ := newCacheKeyTestServerWithDB(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		svr.retryUnloadedIntegrations(ctx, func() []string { return []string{"edge"} }, time.Hour, time.Hour)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retry loop did not stop when its context was done")
	}
}
