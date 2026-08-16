package mcp

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const indexHealthSnapshotTTL = 30 * time.Second

type indexHealthCache struct {
	mu         sync.RWMutex
	payload    map[string]any
	updatedAt  time.Time
	refreshing bool
}

func (s *Server) indexHealthSnapshot() (map[string]any, time.Time, bool) {
	s.indexHealth.mu.RLock()
	defer s.indexHealth.mu.RUnlock()
	if s.indexHealth.payload == nil {
		return nil, time.Time{}, s.indexHealth.refreshing
	}
	return s.indexHealth.payload, s.indexHealth.updatedAt, s.indexHealth.refreshing
}

func (s *Server) refreshIndexHealthInBackground() {
	s.indexHealth.mu.Lock()
	if s.indexHealth.refreshing {
		s.indexHealth.mu.Unlock()
		return
	}
	s.indexHealth.refreshing = true
	s.indexHealth.mu.Unlock()

	go func() {
		payload, err := s.buildIndexHealthPayloadCtx(context.Background())
		s.indexHealth.mu.Lock()
		defer s.indexHealth.mu.Unlock()
		s.indexHealth.refreshing = false
		if err == nil && payload != nil {
			s.indexHealth.payload = payload
			s.indexHealth.updatedAt = time.Now()
		}
	}()
}

func (s *Server) indexHealthNeedsRefresh(updatedAt time.Time) bool {
	return updatedAt.IsZero() || time.Since(updatedAt) >= indexHealthSnapshotTTL
}

func compactIndexHealth(payload map[string]any, updatedAt time.Time, refreshing bool) string {
	stale, _ := payload["stale_files"].([]string)
	failures, _ := payload["parse_failures"].(map[string]string)
	status := "ready"
	if refreshing {
		status = "refreshing"
	}
	return fmt.Sprintf("health=%v nodes=%v stale=%d failures=%d status=%s age=%ds\n",
		payload["health_score"], payload["node_count"], len(stale), len(failures), status, int(time.Since(updatedAt).Seconds()))
}
