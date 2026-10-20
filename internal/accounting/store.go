package accounting

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store records completed operations and serves aggregate queries.
type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	// The caller validates and creates the private state directory; only ordinary files are accepted.
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil, fmt.Errorf("accounting database path must be an on-disk file")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve accounting database path: %w", err)
	}
	parsed := url.URL{Scheme: "file", Path: filepath.ToSlash(absPath)}
	query := parsed.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	parsed.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", parsed.String())
	if err != nil {
		return nil, fmt.Errorf("open accounting database: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect accounting database: %w", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable SQLite WAL: %w", err)
	}
	if err := createSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(absPath, 0600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure accounting database permissions: %w", err)
	}
	return &Store{db: db}, nil
}

func createSchema(db *sql.DB) error {
	// Keep this CHECK in sync with the IncompleteReason constants.
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS accounting_requests (
	started_at_us INTEGER NOT NULL,
	username TEXT NOT NULL,
	model TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL CHECK (kind IN ('create', 'compact')),
	http_status INTEGER NOT NULL,
	response_status TEXT NOT NULL DEFAULT '',
	outcome TEXT NOT NULL,
	input_tokens INTEGER NOT NULL CHECK (input_tokens >= 0),
	output_tokens INTEGER NOT NULL CHECK (output_tokens >= 0),
	total_tokens INTEGER NOT NULL CHECK (total_tokens >= 0),
	cached_tokens INTEGER NOT NULL CHECK (cached_tokens >= 0),
	cache_write_tokens INTEGER NOT NULL CHECK (cache_write_tokens >= 0),
	reasoning_tokens INTEGER NOT NULL CHECK (reasoning_tokens >= 0),
	incomplete_reason TEXT NOT NULL CHECK (incomplete_reason IN (
		'', 'transport_error', 'response_read_error', 'client_disconnected',
		'stream_ended', 'nonterminal_response', 'invalid_response'
	))
)`)
	if err != nil {
		return fmt.Errorf("create accounting schema: %w", err)
	}
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS accounting_started_at_idx ON accounting_requests(started_at_us)`,
		`CREATE INDEX IF NOT EXISTS accounting_model_idx ON accounting_requests(model)`,
		`CREATE INDEX IF NOT EXISTS accounting_user_idx ON accounting_requests(username)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("create accounting index: %w", err)
		}
	}
	return nil
}

func (s *Store) Record(ctx context.Context, meta RequestMeta, result Result) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if meta.User == "" {
		return fmt.Errorf("accounting user is required")
	}
	if meta.Kind != KindCreate && meta.Kind != KindCompact {
		return fmt.Errorf("unsupported accounting operation %q", meta.Kind)
	}
	if meta.StartedAt.IsZero() {
		meta.StartedAt = time.Now()
	}
	model := meta.Model
	if model == "" {
		model = result.ResponseModel
	}
	u := result.Usage
	// A completed operation gets one best-effort insert; failures are returned to the caller.
	_, err := s.db.ExecContext(ctx, `INSERT INTO accounting_requests (
	started_at_us, username, model, kind, http_status, response_status, outcome,
	input_tokens, output_tokens, total_tokens, cached_tokens, cache_write_tokens,
	reasoning_tokens, incomplete_reason
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		meta.StartedAt.UTC().UnixMicro(), meta.User, model, string(meta.Kind),
		result.HTTPStatus, result.Status, result.Outcome,
		u.InputTokens, u.OutputTokens, u.TotalTokens, u.CachedTokens,
		u.CacheWriteTokens, u.ReasoningTokens, string(result.IncompleteReason))
	if err != nil {
		return fmt.Errorf("record accounting result: %w", err)
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Query(ctx context.Context, filters Filters) (Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
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
		where = append(where, "started_at_us >= ?")
		args = append(args, filters.StartUTC.UTC().UnixMicro())
	}
	if filters.EndUTC != nil {
		where = append(where, "started_at_us < ?")
		args = append(args, filters.EndUTC.UTC().UnixMicro())
	}
	query := `SELECT username, model, input_tokens, output_tokens, total_tokens,
cached_tokens, cache_write_tokens, reasoning_tokens FROM accounting_requests WHERE ` + strings.Join(where, " AND ")
	// One SELECT keeps each report on one database snapshot; Go sums remain exact beyond int64.
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Report{}, err
	}
	defer rows.Close()
	grouped := len(filters.GroupBy) != 0
	var total *accumulator
	groups := make(map[groupKey]*accumulator)
	if !grouped {
		total = newAccumulator()
	}
	for rows.Next() {
		var username, model string
		var usage Usage
		if err := rows.Scan(&username, &model,
			&usage.InputTokens, &usage.OutputTokens, &usage.TotalTokens,
			&usage.CachedTokens, &usage.CacheWriteTokens, &usage.ReasoningTokens); err != nil {
			return Report{}, err
		}
		if !grouped {
			total.add(usage)
			continue
		}
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
		groups[key].add(usage)
	}
	if err := rows.Err(); err != nil {
		return Report{}, err
	}
	if !grouped {
		summary := total.summary()
		return Report{Totals: &summary}, nil
	}
	keys := make([]groupKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
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
				value := key.model
				group.Model = &value
			} else if dimension == GroupUser {
				value := key.user
				group.User = &value
			}
		}
		result = append(result, group)
	}
	return Report{Groups: result}, nil
}

