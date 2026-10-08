package auth

import (
	"net/http"
	"strings"
	"sync/atomic"

	"openai-responses-proxy/internal/config"
)

type snapshot struct {
	usersByKey map[string]string
}

type Users struct {
	current atomic.Pointer[snapshot]
}

func NewUsers(users []config.User) *Users {
	result := &Users{}
	result.Replace(users)
	return result
}

// Replace publishes a fully built key map in one atomic swap for concurrent request authentication.
func (u *Users) Replace(users []config.User) {
	next := &snapshot{usersByKey: make(map[string]string)}
	for _, user := range users {
		for _, key := range user.APIKeys {
			next.usersByKey[key] = user.Name
		}
	}
	// Published maps are never mutated, allowing request readers to share a snapshot without a lock.
	u.current.Store(next)
}

// Authenticate accepts exactly one Bearer credential and resolves it against one current snapshot.
func (u *Users) Authenticate(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	// Multiple Authorization fields are rejected to avoid choosing one credential implicitly.
	if len(values) != 1 {
		return "", false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	// Load once so a concurrent reload cannot mix credentials from different snapshots.
	current := u.current.Load()
	if current == nil {
		return "", false
	}
	name, ok := current.usersByKey[parts[1]]
	return name, ok
}
