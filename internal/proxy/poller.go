package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"openai-responses-proxy/internal/accounting"
)

func (s *Server) StartBackground() <-chan error {
	ready := make(chan error, 1)
	// Readiness is reported only after the first durable pending-row scan succeeds.
	s.pollWG.Add(1)
	go s.pollLoop(ready)
	return ready
}

// StopBackground waits for recovery work, bounded by ctx, then closes idle upstream connections.
// The caller must cancel serviceCtx to stop the poll loop and active poll workers.
func (s *Server) StopBackground(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.pollWG.Wait()
		close(done)
	}()
	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	s.transport.CloseIdleConnections()
	return err
}

func (s *Server) pollLoop(ready chan error) {
	defer s.pollWG.Done()
	ticker := time.NewTicker(s.config.Background.PollInterval)
	defer ticker.Stop()
	startup := true
	readySent := false
	defer func() {
		if !readySent {
			ready <- s.serviceCtx.Err()
			close(ready)
		}
	}()
	for {
		if err := s.serviceCtx.Err(); err != nil {
			return
		}
		pending, err := s.accounting.Pending(s.serviceCtx)
		if err != nil {
			// Keep retrying the scan; readiness remains pending until persisted work is visible.
			s.logger.Error("could not list pending accounting rows", "error", err.Error())
		} else {
			for _, item := range pending {
				if item.ResponseID == "" {
					if startup {
						// A restart cannot resume work when no upstream response ID was saved.
						// Mark the row incomplete instead of leaving it pending forever.
						s.markIncomplete(item.ID, "service restarted before the upstream response ID was saved")
					}
					continue
				}
				s.pollMu.Lock()
				_, recovery := s.recover[item.ResponseID]
				if startup && !item.Background {
					s.recover[item.ResponseID] = struct{}{}
					recovery = true
				}
				s.pollMu.Unlock()
				if startup || item.Background || recovery {
					// Startup resumes saved work; later scans poll background and interrupted foreground work.
					s.startPoll(item)
				}
			}
			if startup {
				startup = false
				ready <- nil
				close(ready)
				readySent = true
			}
		}
		select {
		case <-s.serviceCtx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) startPoll(item accounting.PendingRequest) {
	select {
	case s.pollSem <- struct{}{}:
	case <-s.serviceCtx.Done():
		return
	default:
		// A full pool is retried on the next scan instead of spawning unbounded goroutines.
		return
	}
	s.pollMu.Lock()
	if _, active := s.polling[item.ResponseID]; active {
		// Multiple scans can find one row while its existing poll is still running.
		s.pollMu.Unlock()
		<-s.pollSem
		return
	}
	s.polling[item.ResponseID] = struct{}{}
	s.pollWG.Add(1)
	s.pollMu.Unlock()
	go func() {
		defer s.pollWG.Done()
		defer func() {
			s.pollMu.Lock()
			delete(s.polling, item.ResponseID)
			s.pollMu.Unlock()
			<-s.pollSem
		}()
		s.pollOne(item)
	}()
}

func (s *Server) pollOne(item accounting.PendingRequest) {
	// Recovery belongs to the service lifetime, not the client request that created the row.
	requestContext, cancel := responseContext(s.serviceCtx, s.config.Timeouts.TotalRequest)
	defer cancel()
	responseID := item.ResponseID
	// Escape the persisted ID as one path segment so it cannot alter the polling route.
	path := "/v1/responses/" + url.PathEscape(responseID)
	requestURL, err := url.Parse(path)
	if err != nil {
		s.markIncomplete(item.ID, "invalid saved upstream response ID")
		return
	}
	response, err, _ := s.doWithRetries(requestContext, http.MethodGet, s.targetURL(requestURL), nil, nil)
	if err != nil {
		if !errors.Is(err, context.Canceled) || s.serviceCtx.Err() == nil {
			s.logger.UpstreamError("background response poll failed", "error", err.Error())
		}
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		// A missing saved response cannot be recovered by a later poll.
		s.markIncompleteFor(item.ID, responseID, "upstream response is no longer available")
		s.untrackRecovery(responseID)
		return
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
		// These failures may be transient, so leave the row pending for the next scan.
		s.logger.UpstreamError("background response poll returned an upstream error", "status", response.StatusCode)
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		s.logger.UpstreamError("background response poll returned an upstream error", "status", response.StatusCode)
		s.markIncompleteFor(item.ID, responseID, fmt.Sprintf("upstream response poll returned HTTP %d", response.StatusCode))
		s.untrackRecovery(responseID)
		return
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		s.logger.UpstreamError("background response data ended early", "error", err.Error())
		return
	}
	info := parseResponse(body)
	if !info.Valid {
		s.markIncompleteFor(item.ID, responseID, "upstream response could not be recovered")
		s.untrackRecovery(responseID)
		return
	}
	if info.ID == "" {
		info.ID = responseID
	}
	if isTerminalStatus(info.Status) {
		s.finalize(item.ID, info, response.StatusCode, info.Status)
		s.untrackRecovery(responseID)
		return
	}
	if !isPendingStatus(info.Status) {
		s.markIncompleteFor(item.ID, responseID, "upstream response has an unknown status")
		s.untrackRecovery(responseID)
		return
	}
	// Preserve usage observed while the background response is still running.
	snapshot := info.snapshot(accounting.PhaseProvisional, response.StatusCode, "")
	if _, err := s.accounting.ObserveByResponseID(s.persistContext(), responseID, snapshot); err != nil {
		s.logger.Error("could not observe background response usage", "error", err.Error())
	}
}

func (s *Server) markIncompleteFor(id accounting.RequestID, responseID, reason string) {
	if err := s.accounting.MarkIncomplete(s.persistContext(), id, responseID, reason); err != nil {
		s.logger.Error("could not mark recovered accounting row incomplete", "error", err.Error())
	}
}

func (s *Server) untrackRecovery(id string) {
	s.pollMu.Lock()
	delete(s.recover, id)
	s.pollMu.Unlock()
}
