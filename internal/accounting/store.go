package accounting

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	usagePending     = "pending"
	usageComplete    = "complete"
	usageIncomplete  = "incomplete"
	writeRetryPeriod = time.Second
)

type writeOperation func(context.Context) error

// Store serializes row updates and retains transient failures until SQLite recovers.
type Store struct {
	db *sql.DB

	writeMu         sync.Mutex
	admissionClosed bool
	closing         bool
	closed          bool
	queuedWrites    []writeOperation
	retryWake       chan struct{}
	stopRetry       chan struct{}
	retryDone       chan struct{}
	retryCtx        context.Context
	retryCancel     context.CancelFunc
	closeOnce       sync.Once
	closeErr        error
}

// Open initializes SQLite storage, applies the schema, and starts the worker
// that retries retained writes after transient database failures.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("accounting database path is required")
	}
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		// New accounting directories use private permissions within the configured state path.
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, fmt.Errorf("create accounting directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open accounting database: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect accounting database: %w", err)
	}
	// WAL lets aggregate readers proceed alongside the store's serialized accounting updates.
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable SQLite WAL: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		// Keep the database file private even when it was created in an existing directory.
		if err := os.Chmod(path, 0600); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("secure accounting database permissions: %w", err)
		}
	}
	retryCtx, retryCancel := context.WithCancel(context.Background())
	store := &Store{
		db: db, retryWake: make(chan struct{}, 1), stopRetry: make(chan struct{}), retryDone: make(chan struct{}),
		retryCtx: retryCtx, retryCancel: retryCancel,
	}
	go store.retryWrites()
	return store, nil
}

// sqliteDSN builds a file URI and applies connection-local SQLite settings.
func sqliteDSN(path string) string {
	var dsn string
	if path == ":memory:" {
		// A shared-memory URI lets the configured connection pool see one in-memory database.
		dsn = "file:responses-accounting?mode=memory&cache=shared"
	} else if strings.HasPrefix(path, "file:") {
		dsn = path
	} else {
		absolute, err := filepath.Abs(path)
		if err != nil {
			absolute = path
		}
		dsn = (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}).String()
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	query := parsed.Query()
	// Keep lock waits bounded and enforce SQLite constraints on every pooled connection.
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// migrate creates the durable request ledger and the indexes used by recovery and queries.
func migrate(db *sql.DB) error {
	// Constraints keep durable row states and token counters valid even after recovery writes.
	statements := []string{
		`CREATE TABLE IF NOT EXISTS accounting_requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	started_at_us INTEGER NOT NULL,
	username TEXT NOT NULL,
	model TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL CHECK (kind IN ('create', 'compact')),
	background INTEGER NOT NULL DEFAULT 0 CHECK (background IN (0, 1)),
	response_id TEXT,
	request_outcome TEXT NOT NULL DEFAULT 'pending',
	http_status INTEGER NOT NULL DEFAULT 0,
	response_status TEXT NOT NULL DEFAULT '',
	usage_state TEXT NOT NULL CHECK (usage_state IN ('pending', 'complete', 'incomplete')),
	usage_known INTEGER NOT NULL DEFAULT 0 CHECK (usage_known IN (0, 1)),
	input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
	output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
	total_tokens INTEGER NOT NULL DEFAULT 0 CHECK (total_tokens >= 0),
	cached_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cached_tokens >= 0),
	cache_write_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0),
	reasoning_tokens INTEGER NOT NULL DEFAULT 0 CHECK (reasoning_tokens >= 0),
	incomplete_reason TEXT NOT NULL DEFAULT ''
)`,
		// A response ID is a unique recovery key, while null IDs remain valid before upstream assigns one.
		`CREATE UNIQUE INDEX IF NOT EXISTS accounting_response_id_unique ON accounting_requests(response_id) WHERE response_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS accounting_started_at_idx ON accounting_requests(started_at_us)`,
		`CREATE INDEX IF NOT EXISTS accounting_model_idx ON accounting_requests(model)`,
		`CREATE INDEX IF NOT EXISTS accounting_user_idx ON accounting_requests(username)`,
		`CREATE INDEX IF NOT EXISTS accounting_state_idx ON accounting_requests(usage_state, background)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("migrate accounting database: %w", err)
		}
	}
	return nil
}

// Close drains accounting writes with the store's default ten-second deadline.
func (s *Store) Close() error {
	// Keep the convenience close bounded; service shutdown can supply its configured deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.CloseContext(ctx)
}

// CloseContext flushes queued updates before closing the database, bounded by ctx.
// If ctx expires before shutdown completes, it leaves the handle open rather than exceed the deadline.
func (s *Store) CloseContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.closeOnce.Do(func() {
		flushErr := s.Flush(ctx)
		// Flush cancels periodic retries; now wait for the worker before touching the DB handle.
		close(s.stopRetry)
		select {
		case <-s.retryDone:
		case <-ctx.Done():
			if flushErr == nil {
				flushErr = fmt.Errorf("accounting retry worker did not stop before shutdown deadline: %w", ctx.Err())
			}
			s.closeErr = fmt.Errorf("accounting database remains open after shutdown deadline: %w", flushErr)
			return
		}
		// Wait for active writes before closing the DB; otherwise leave its handle usable.
		if !lockWriteContext(ctx, &s.writeMu) {
			if flushErr == nil {
				flushErr = fmt.Errorf("accounting writer did not stop before shutdown deadline: %w", ctx.Err())
			}
			s.closeErr = fmt.Errorf("accounting database remains open after shutdown deadline: %w", flushErr)
			return
		}
		if ctx.Err() != nil {
			s.writeMu.Unlock()
			if flushErr == nil {
				flushErr = ctx.Err()
			}
			s.closeErr = fmt.Errorf("accounting database remains open after shutdown deadline: %w", flushErr)
			return
		}
		// Report leftover operations even when the database itself can still be closed cleanly.
		pending := len(s.queuedWrites)
		if pending != 0 && flushErr == nil {
			flushErr = fmt.Errorf("%d queued writes remain", pending)
		}
		s.closed = true
		closeErr := s.db.Close()
		s.writeMu.Unlock()
		if flushErr != nil {
			s.closeErr = fmt.Errorf("accounting database closed with unflushed writes: %w", flushErr)
		}
		if closeErr != nil {
			if s.closeErr == nil {
				s.closeErr = closeErr
			} else {
				s.closeErr = fmt.Errorf("%v; close accounting database: %w", s.closeErr, closeErr)
			}
		}
	})
	return s.closeErr
}

// Flush retries retained accounting updates until the queue drains or ctx expires.
// Call it after request and background workers have stopped accepting new writes.
func (s *Store) Flush(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.retryCancel()
	if !lockWriteContext(ctx, &s.writeMu) {
		return s.flushError(ctx, ctx.Err())
	}
	// Stop new admissions and updates before draining the retained queue.
	s.closing = true
	s.writeMu.Unlock()
	for {
		if !lockWriteContext(ctx, &s.writeMu) {
			return s.flushError(ctx, ctx.Err())
		}
		if s.closed {
			s.writeMu.Unlock()
			return ErrUnavailable
		}
		if len(s.queuedWrites) == 0 {
			if !s.admissionClosed {
				s.writeMu.Unlock()
				return nil
			}
			pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			// An empty queue is insufficient if a failed admission left storage unavailable.
			err := s.db.PingContext(pingCtx)
			cancel()
			if err == nil {
				s.admissionClosed = false
				s.writeMu.Unlock()
				return nil
			}
			s.writeMu.Unlock()
			if !waitForRetry(ctx) {
				return s.flushError(ctx, ctx.Err())
			}
			continue
		}

		writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		// Process in order so later state changes cannot overtake an earlier retry.
		err := s.queuedWrites[0](writeCtx)
		cancel()
		if err == nil || isTerminalWriteError(err) {
			s.queuedWrites[0] = nil
			s.queuedWrites = s.queuedWrites[1:]
		}
		if len(s.queuedWrites) == 0 {
			s.admissionClosed = false
		}
		remaining := len(s.queuedWrites)
		s.writeMu.Unlock()
		if err != nil && !isTerminalWriteError(err) {
			if ctx.Err() != nil {
				return s.flushError(ctx, ctx.Err())
			}
			if !waitForRetry(ctx) {
				return s.flushError(ctx, ctx.Err())
			}
			continue
		}
		if remaining == 0 {
			return nil
		}
	}
}

// flushError adds queue or storage state so callers can tell why draining stopped.
func (s *Store) flushError(ctx context.Context, err error) error {
	if !lockWriteContext(ctx, &s.writeMu) {
		return fmt.Errorf("accounting flush could not inspect pending writes before shutdown deadline: %w", err)
	}
	pending := len(s.queuedWrites)
	admissionClosed := s.admissionClosed
	s.writeMu.Unlock()
	if pending != 0 {
		return fmt.Errorf("accounting flush stopped with %d queued writes: %w", pending, err)
	}
	if admissionClosed {
		return fmt.Errorf("accounting storage remained unavailable during flush: %w", err)
	}
	return err
}

func lockWriteContext(ctx context.Context, mu *sync.Mutex) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if mu.TryLock() {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		// Mutex has no context-aware lock operation, so poll without blocking past shutdown.
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

// waitForRetry pauses between bounded storage attempts and exits promptly on shutdown.
func waitForRetry(ctx context.Context) bool {
	timer := time.NewTimer(writeRetryPeriod)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Admit durably creates the operation row before upstream work can begin.
func (s *Store) Admit(ctx context.Context, meta RequestMeta) (RequestID, error) {
	if meta.User == "" {
		return 0, fmt.Errorf("accounting user is required")
	}
	if meta.Kind != KindCreate && meta.Kind != KindCompact {
		return 0, fmt.Errorf("unsupported accounting operation %q", meta.Kind)
	}
	if meta.StartedAt.IsZero() {
		// Admission time is captured before upstream work and persisted consistently in UTC.
		meta.StartedAt = time.Now().UTC()
	}
	meta.StartedAt = meta.StartedAt.UTC()
	var id RequestID
	s.writeMu.Lock()
	if s.closing || s.closed {
		s.writeMu.Unlock()
		return 0, ErrUnavailable
	}
	if s.admissionClosed {
		s.writeMu.Unlock()
		return 0, ErrUnavailable
	}
	// Commit this row before sending upstream so retries and polling never admit duplicates.
	writeCtx, cancel := boundedWriteContext(ctx)
	tx, err := s.db.BeginTx(writeCtx, nil)
	if err == nil {
		var result sql.Result
		result, err = tx.ExecContext(writeCtx, `
INSERT INTO accounting_requests
	(started_at_us, username, model, kind, background, usage_state)
VALUES (?, ?, ?, ?, ?, 'pending')`,
			meta.StartedAt.UnixMicro(), meta.User, meta.Model, string(meta.Kind), meta.Background)
		if err == nil {
			lastID, idErr := result.LastInsertId()
			err = idErr
			id = RequestID(lastID)
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
	}
	cancel()
	if err != nil {
		// Never replay an uncertain admission: the caller needs a confirmed row ID before upstream work.
		s.admissionClosed = true
		s.writeMu.Unlock()
		s.wakeRetry()
		return 0, fmt.Errorf("accounting admission failed: %w", err)
	}
	s.writeMu.Unlock()
	return id, nil
}

// AttachResponseID binds an admitted row to the upstream ID used for later recovery.
func (s *Store) AttachResponseID(ctx context.Context, id RequestID, responseID string) error {
	if responseID == "" {
		return fmt.Errorf("response ID is required")
	}
	return s.write(ctx, func(ctx context.Context) error {
		return s.attachResponseID(ctx, id, responseID)
	})
}

func (s *Store) attachResponseID(ctx context.Context, id RequestID, responseID string) error {
	var current sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT response_id FROM accounting_requests WHERE id = ?`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if current.Valid && current.String != responseID {
		return fmt.Errorf("accounting request already has a different response ID")
	}
	// One upstream ID can recover at most one admitted row, even across process restarts.
	var otherID RequestID
	err = s.db.QueryRowContext(ctx, `SELECT id FROM accounting_requests WHERE response_id = ? AND id <> ?`, responseID, id).Scan(&otherID)
	if err == nil {
		return ErrResponseIDConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE accounting_requests SET response_id = ? WHERE id = ?`, responseID, id)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: accounting_requests.response_id") {
		return ErrResponseIDConflict
	}
	return err
}

// ObserveByResponseID applies a response observation to its durable recovery row.
// The bool reports whether this store owns the supplied response ID.
func (s *Store) ObserveByResponseID(ctx context.Context, responseID string, snapshot Snapshot) (bool, error) {
	if responseID == "" {
		return false, nil
	}
	if snapshot.Phase != PhaseProvisional && snapshot.Phase != PhaseFinal {
		return false, fmt.Errorf("snapshot phase must be provisional or final")
	}
	var id RequestID
	err := s.db.QueryRowContext(ctx, `SELECT id FROM accounting_requests WHERE response_id = ?`, responseID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, s.retryByResponseID(ctx, responseID, snapshot)
	}
	if snapshot.HTTPStatus == 404 && snapshot.Status == "not_found" {
		// A missing background response can no longer produce final usage.
		return true, s.MarkIncomplete(ctx, id, responseID, "upstream response returned 404")
	}
	err = s.write(ctx, func(ctx context.Context) error {
		return s.applySnapshot(ctx, id, snapshot, responseID)
	})
	return true, err
}

func (s *Store) retryByResponseID(ctx context.Context, responseID string, snapshot Snapshot) error {
	return s.write(ctx, func(ctx context.Context) error {
		// A queued ID attachment may become visible while this observation waits for storage recovery.
		var id RequestID
		err := s.db.QueryRowContext(ctx, `SELECT id FROM accounting_requests WHERE response_id = ?`, responseID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return s.applySnapshot(ctx, id, snapshot, responseID)
	})
}

// Finalize applies a terminal observation, retaining stored counters when usage is absent.
func (s *Store) Finalize(ctx context.Context, id RequestID, snapshot Snapshot) error {
	// Finalization is terminal even when the caller supplied a provisional snapshot value.
	snapshot.Phase = PhaseFinal
	return s.write(ctx, func(ctx context.Context) error {
		return s.applySnapshot(ctx, id, snapshot, snapshot.ResponseID)
	})
}

func (s *Store) applySnapshot(ctx context.Context, id RequestID, snapshot Snapshot, responseID string) error {
	if snapshot.Phase != PhaseProvisional && snapshot.Phase != PhaseFinal {
		return fmt.Errorf("snapshot phase must be provisional or final")
	}
	if responseID != "" {
		// Recovery identity is immutable, and the unique index prevents cross-row reassignment.
		var current sql.NullString
		err := s.db.QueryRowContext(ctx, `SELECT response_id FROM accounting_requests WHERE id = ?`, id).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if current.Valid && current.String != responseID {
			return fmt.Errorf("accounting request already has a different response ID")
		}
		var otherID RequestID
		err = s.db.QueryRowContext(ctx, `SELECT id FROM accounting_requests WHERE response_id = ? AND id <> ?`, responseID, id).Scan(&otherID)
		if err == nil {
			return ErrResponseIDConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if snapshot.Phase == PhaseFinal {
		// Infer an outcome only when the caller has no more specific terminal classification.
		if snapshot.Outcome == "" {
			if snapshot.Status != "" {
				snapshot.Outcome = snapshot.Status
			} else if snapshot.HTTPStatus >= 400 {
				snapshot.Outcome = "failed"
			} else {
				snapshot.Outcome = "complete"
			}
		}
	}
	if !validUsage(snapshot.Usage) {
		// Invalid counters are recorded as zero to preserve the schema's nonnegative invariant.
		snapshot.Usage = Usage{}
	}
	usage := snapshot.Usage
	usageKnown := snapshot.UsageKnown
	var previousKnown bool
	var previousUsage Usage
	if !snapshot.UsageKnown {
		// A later event without usage must not erase a previously observed usage object.
		err := s.db.QueryRowContext(ctx, `
SELECT usage_known, input_tokens, output_tokens, total_tokens,
	cached_tokens, cache_write_tokens, reasoning_tokens
FROM accounting_requests WHERE id = ?`, id).Scan(
			&previousKnown,
			&previousUsage.InputTokens, &previousUsage.OutputTokens, &previousUsage.TotalTokens,
			&previousUsage.CachedTokens, &previousUsage.CacheWriteTokens, &previousUsage.ReasoningTokens)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if previousKnown {
			usage = previousUsage
			usageKnown = true
		} else {
			usage = Usage{}
			usageKnown = false
		}
	}
	// Merge fields instead of replacing the row so provisional observations cannot undo final state.
	_, err := s.db.ExecContext(ctx, `
UPDATE accounting_requests SET
	response_id = COALESCE(response_id, NULLIF(?, '')),
	model = CASE WHEN model = '' AND ? = 'final' AND ? <> '' THEN ? ELSE model END,
	request_outcome = CASE WHEN ? <> '' THEN ? ELSE request_outcome END,
	http_status = CASE WHEN ? > 0 THEN ? ELSE http_status END,
	response_status = CASE WHEN ? <> '' THEN ? ELSE response_status END,
	usage_state = CASE WHEN ? = 'final' THEN 'complete' ELSE usage_state END,
	usage_known = ?,
	input_tokens = ?, output_tokens = ?, total_tokens = ?,
	cached_tokens = ?, cache_write_tokens = ?, reasoning_tokens = ?,
	incomplete_reason = CASE WHEN ? = 'final' THEN '' ELSE incomplete_reason END
WHERE id = ? AND (? = 'final' OR usage_state = 'pending')`,
		responseID,
		string(snapshot.Phase), snapshot.ResponseModel, snapshot.ResponseModel,
		snapshot.Outcome, snapshot.Outcome,
		snapshot.HTTPStatus, snapshot.HTTPStatus,
		snapshot.Status, snapshot.Status,
		string(snapshot.Phase),
		usageKnown,
		usage.InputTokens, usage.OutputTokens, usage.TotalTokens,
		usage.CachedTokens, usage.CacheWriteTokens, usage.ReasoningTokens,
		string(snapshot.Phase), id, string(snapshot.Phase))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: accounting_requests.response_id") {
			return ErrResponseIDConflict
		}
		return err
	}
	if snapshot.Phase == PhaseFinal {
		// Repeated final writes are allowed; verify existence separately to report missing IDs.
		var exists int
		err = s.db.QueryRowContext(ctx, `SELECT 1 FROM accounting_requests WHERE id = ?`, id).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// MarkIncomplete records that recovery ended without a terminal usage snapshot.
// A completed row remains complete when a later poll reports an error.
func (s *Store) MarkIncomplete(ctx context.Context, id RequestID, responseID, reason string) error {
	return s.write(ctx, func(ctx context.Context) error {
		if responseID != "" {
			// Keep the recovery key bound to the row it identified before recording failure.
			var current sql.NullString
			err := s.db.QueryRowContext(ctx, `SELECT response_id FROM accounting_requests WHERE id = ?`, id).Scan(&current)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if current.Valid && current.String != responseID {
				return fmt.Errorf("accounting request already has a different response ID")
			}
			var otherID RequestID
			err = s.db.QueryRowContext(ctx, `SELECT id FROM accounting_requests WHERE response_id = ? AND id <> ?`, responseID, id).Scan(&otherID)
			if err == nil {
				return ErrResponseIDConflict
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		// Preserve already finalized usage if a later poll encounters an unrelated failure.
		result, err := s.db.ExecContext(ctx, `
UPDATE accounting_requests SET
	response_id = COALESCE(response_id, NULLIF(?, '')),
	usage_state = CASE WHEN usage_state = 'complete' THEN usage_state ELSE 'incomplete' END,
	request_outcome = CASE WHEN request_outcome = 'pending' THEN 'incomplete' ELSE request_outcome END,
	incomplete_reason = CASE WHEN usage_state = 'complete' THEN incomplete_reason ELSE ? END
WHERE id = ?`, responseID, reason, id)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed: accounting_requests.response_id") {
				return ErrResponseIDConflict
			}
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func validUsage(usage Usage) bool {
	return usage.InputTokens >= 0 && usage.OutputTokens >= 0 && usage.TotalTokens >= 0 &&
		usage.CachedTokens >= 0 && usage.CacheWriteTokens >= 0 && usage.ReasoningTokens >= 0
}

// write serializes updates and retains database failures for retry after storage recovers.
func (s *Store) write(ctx context.Context, operation writeOperation) error {
	s.writeMu.Lock()
	if s.closing || s.closed {
		s.writeMu.Unlock()
		return ErrUnavailable
	}
	if s.admissionClosed {
		// Preserve write order while the worker probes storage and drains earlier failures.
		s.queuedWrites = append(s.queuedWrites, operation)
		s.writeMu.Unlock()
		s.wakeRetry()
		return nil
	}
	writeCtx, cancel := boundedWriteContext(ctx)
	err := operation(writeCtx)
	cancel()
	if err == nil {
		s.writeMu.Unlock()
		return nil
	}
	if !isDatabaseFailure(err) {
		// Identity conflicts and missing rows are permanent; retrying them cannot repair state.
		s.writeMu.Unlock()
		return err
	}
	s.admissionClosed = true
	// Treat transient failures as accepted work; replay it after SQLite becomes available.
	s.queuedWrites = append(s.queuedWrites, operation)
	s.writeMu.Unlock()
	s.wakeRetry()
	return nil
}

func isDatabaseFailure(err error) bool {
	return !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrResponseIDConflict) &&
		!strings.Contains(err.Error(), "different response ID")
}

func boundedWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx != nil {
		// Bound persistence work so caller cancellation cannot interrupt a transaction midway.
		return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	}
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// wakeRetry coalesces notifications; the worker's ticker remains a fallback.
func (s *Store) wakeRetry() {
	select {
	case s.retryWake <- struct{}{}:
	default:
	}
}

// retryWrites replays retained operations in order and probes an empty queue for recovery.
func (s *Store) retryWrites() {
	defer close(s.retryDone)
	ticker := time.NewTicker(writeRetryPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopRetry:
			return
		case <-s.retryCtx.Done():
			return
		case <-s.retryWake:
		case <-ticker.C:
		}
		s.writeMu.Lock()
		if !s.admissionClosed {
			s.writeMu.Unlock()
			continue
		}
		if len(s.queuedWrites) == 0 {
			// Reopen admissions only after SQLite responds again, even when no update is queued.
			ctx, cancel := context.WithTimeout(s.retryCtx, 5*time.Second)
			err := s.db.PingContext(ctx)
			cancel()
			if err == nil {
				s.admissionClosed = false
			}
			s.writeMu.Unlock()
			continue
		}
		ctx, cancel := context.WithTimeout(s.retryCtx, 10*time.Second)
		// Retry only the head; serialized replay keeps state transitions ordered and idempotent.
		err := s.queuedWrites[0](ctx)
		cancel()
		if err == nil || isTerminalWriteError(err) {
			s.queuedWrites[0] = nil
			s.queuedWrites = s.queuedWrites[1:]
		}
		if len(s.queuedWrites) == 0 {
			s.admissionClosed = false
		}
		s.writeMu.Unlock()
	}
}

// isTerminalWriteError identifies replay failures that cannot be fixed by retrying.
func isTerminalWriteError(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrResponseIDConflict) || strings.Contains(err.Error(), "different response ID")
}

// Pending returns rows that still need upstream recovery, ordered for reproducible startup work.
func (s *Store) Pending(ctx context.Context) ([]PendingRequest, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, response_id, username, model, started_at_us, kind, background
FROM accounting_requests WHERE usage_state = 'pending' ORDER BY started_at_us, id`)
	if err != nil {
		s.closeAdmissionAfterReadError(err)
		return nil, err
	}
	defer rows.Close()
	var pending []PendingRequest
	// Nullable response IDs are retained because a crash can occur before upstream assigns one.
	for rows.Next() {
		var item PendingRequest
		var responseID sql.NullString
		var startedAt int64
		var background bool
		if err := rows.Scan(&item.ID, &responseID, &item.User, &item.Model, &startedAt, &item.Kind, &background); err != nil {
			s.closeAdmissionAfterReadError(err)
			return nil, err
		}
		if responseID.Valid {
			item.ResponseID = responseID.String
		}
		item.StartedAt = time.UnixMicro(startedAt).UTC()
		item.Background = background
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		s.closeAdmissionAfterReadError(err)
		return nil, err
	}
	return pending, nil
}

func (s *Store) closeAdmission() {
	// Stop new ledger admissions until the retry worker confirms storage is available.
	s.writeMu.Lock()
	s.admissionClosed = true
	s.writeMu.Unlock()
	s.wakeRetry()
}

func (s *Store) closeAdmissionAfterReadError(err error) {
	// Request cancellation is local to this read and does not indicate a storage outage.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	s.closeAdmission()
}

// Query returns aggregates from one read transaction; raw request rows are never exposed.
func (s *Store) Query(ctx context.Context, filters Filters) (Report, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		s.closeAdmissionAfterReadError(err)
		return Report{}, err
	}
	defer tx.Rollback()
	where := []string{"1 = 1"}
	args := make([]any, 0, 4)
	if filters.Model != "" {
		where = append(where, "model = ?")
		args = append(args, filters.Model)
	}
	if filters.User != "" {
		where = append(where, "username = ?")
		args = append(args, filters.User)
	}
	if filters.StartUTC != nil {
		// The start is inclusive; paired with the strict end this forms a half-open range.
		where = append(where, "started_at_us >= ?")
		args = append(args, filters.StartUTC.UTC().UnixMicro())
	}
	if filters.EndUTC != nil {
		where = append(where, "started_at_us < ?")
		args = append(args, filters.EndUTC.UTC().UnixMicro())
	}
	query := `SELECT username, model, usage_state, input_tokens, output_tokens, total_tokens,
cached_tokens, cache_write_tokens, reasoning_tokens FROM accounting_requests WHERE ` + strings.Join(where, " AND ")
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		s.closeAdmissionAfterReadError(err)
		return Report{}, err
	}
	defer rows.Close()
	if len(filters.GroupBy) == 0 {
		// Keep ungrouped empty results explicit as zero totals rather than a missing report field.
		total := newAccumulator()
		for rows.Next() {
			var username, model, state string
			var usage Usage
			if err := rows.Scan(&username, &model, &state,
				&usage.InputTokens, &usage.OutputTokens, &usage.TotalTokens,
				&usage.CachedTokens, &usage.CacheWriteTokens, &usage.ReasoningTokens); err != nil {
				s.closeAdmissionAfterReadError(err)
				return Report{}, err
			}
			total.add(state, usage)
		}
		if err := rows.Err(); err != nil {
			s.closeAdmissionAfterReadError(err)
			return Report{}, err
		}
		if err := rows.Close(); err != nil {
			s.closeAdmissionAfterReadError(err)
			return Report{}, err
		}
		summary := total.summary()
		if err := tx.Commit(); err != nil {
			s.closeAdmissionAfterReadError(err)
			return Report{}, err
		}
		return Report{Totals: &summary}, nil
	}

	groups := make(map[groupKey]*accumulator)
	for rows.Next() {
		var username, model, state string
		var usage Usage
		if err := rows.Scan(&username, &model, &state,
			&usage.InputTokens, &usage.OutputTokens, &usage.TotalTokens,
			&usage.CachedTokens, &usage.CacheWriteTokens, &usage.ReasoningTokens); err != nil {
			s.closeAdmissionAfterReadError(err)
			return Report{}, err
		}
		// Project each row onto only the requested dimensions so omitted fields collapse together.
		key := groupKey{}
		for _, dimension := range filters.GroupBy {
			switch dimension {
			case GroupModel:
				key.model = model
			case GroupUser:
				key.user = username
			}
		}
		if groups[key] == nil {
			groups[key] = newAccumulator()
		}
		groups[key].add(state, usage)
	}
	if err := rows.Err(); err != nil {
		s.closeAdmissionAfterReadError(err)
		return Report{}, err
	}
	if err := rows.Close(); err != nil {
		s.closeAdmissionAfterReadError(err)
		return Report{}, err
	}
	if err := tx.Commit(); err != nil {
		s.closeAdmissionAfterReadError(err)
		return Report{}, err
	}
	keys := make([]groupKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		// Stable model/user ordering makes report output deterministic across map iteration order.
		if keys[i].model == keys[j].model {
			return keys[i].user < keys[j].user
		}
		return keys[i].model < keys[j].model
	})
	result := make([]Group, 0, len(keys))
	for _, key := range keys {
		group := Group{Summary: groups[key].summary()}
		for _, dimension := range filters.GroupBy {
			if dimension == GroupModel {
				model := key.model
				group.Model = &model
			} else if dimension == GroupUser {
				user := key.user
				group.User = &user
			}
		}
		result = append(result, group)
	}
	return Report{Groups: result}, nil
}
