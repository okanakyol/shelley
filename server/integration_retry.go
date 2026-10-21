package server

import (
	"context"
	"time"
)

// Delays between retries of unloaded integrations: the first retry comes
// soon, since a proxy that starts with the machine is usually up within
// seconds, and later ones back off so a proxy that stays down costs little.
const (
	integrationRetryMin = 15 * time.Second
	integrationRetryMax = 5 * time.Minute
)

// RetryUnloadedIntegrations refreshes the model catalog while unloaded
// reports shelley.json LLM integrations that could not be loaded, such as a
// credential proxy that started after Shelley. Without it their models only
// appear after a manual refresh, and the refresh control is hidden while the
// model list is empty. It returns when ctx is done.
func (s *Server) RetryUnloadedIntegrations(ctx context.Context, unloaded func() []string) {
	if unloaded == nil || s.refreshBuiltModels == nil {
		return
	}
	s.retryUnloadedIntegrations(ctx, unloaded, integrationRetryMin, integrationRetryMax)
}

func (s *Server) retryUnloadedIntegrations(ctx context.Context, unloaded func() []string, minDelay, maxDelay time.Duration) {
	delay := minDelay
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		// A manual refresh may have loaded them meanwhile, or dropped one
		// whose proxy went away; either way the latest result decides.
		names := unloaded()
		if len(names) == 0 {
			delay = minDelay
			continue
		}
		if err := s.refreshModels(ctx); err != nil {
			s.logger.Warn("Retrying unloaded LLM integrations failed", "integrations", names, "error", err)
		}
		if still := unloaded(); len(still) > 0 {
			if delay *= 2; delay > maxDelay {
				delay = maxDelay
			}
			s.logger.Info("LLM integrations still unloaded; will retry", "integrations", still, "in", delay)
		} else {
			s.logger.Info("Loaded LLM integrations after retry", "integrations", names)
			delay = minDelay
		}
	}
}
