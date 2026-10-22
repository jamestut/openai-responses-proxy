package auth

import (
	"net/http/httptest"
	"sync"
	"testing"

	"openai-responses-proxy/internal/config"
)

// Bearer parsing accepts one known key, rejects malformed or repeated headers, and replacement activates only the new key set.
func TestAuthenticateAndReplace(t *testing.T) {
	users := NewUsers([]config.User{
		{Name: "alice", APIKeys: []string{"alice-one", "alice-two"}},
		{Name: "bob", APIKeys: []string{"bob-one"}},
	})

	for _, tc := range []struct {
		header string
		want   string
		ok     bool
	}{
		{header: "Bearer alice-one", want: "alice", ok: true},
		{header: "Bearer alice-two", want: "alice", ok: true},
		{header: "bearer bob-one", want: "bob", ok: true},
		{header: "", ok: false},
		{header: "Bearer unknown"},
		{header: "Basic alice-one"},
		{header: "Bearer"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if tc.header != "" {
			r.Header.Add("Authorization", tc.header)
		}
		got, ok := users.Authenticate(r)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Authenticate(%q) = (%q, %v), want (%q, %v)", tc.header, got, ok, tc.want, tc.ok)
		}
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Add("Authorization", "Bearer alice-one")
	r.Header.Add("Authorization", "Bearer bob-one")
	if _, ok := users.Authenticate(r); ok {
		t.Fatal("accepted multiple Authorization values")
	}

	users.Replace([]config.User{{Name: "alice", APIKeys: []string{"alice-new"}}})
	for _, key := range []string{"alice-one", "alice-two", "bob-one"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		if _, ok := users.Authenticate(r); ok {
			t.Errorf("old key %q remained active after replacement", key)
		}
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer alice-new")
	if got, ok := users.Authenticate(r); !ok || got != "alice" {
		t.Fatalf("replacement key authenticated as (%q, %v)", got, ok)
	}
}

// Keep one key in every replacement while readers authenticate concurrently.
// It must always resolve to Alice or Bob, never disappear during a swap.
func TestConcurrentAuthenticateDuringReplace(t *testing.T) {
	alice := []config.User{{Name: "alice", APIKeys: []string{"shared"}}}
	bob := []config.User{{Name: "bob", APIKeys: []string{"shared"}}}
	users := NewUsers(alice)
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer shared")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			if i%2 == 0 {
				users.Replace(bob)
			} else {
				users.Replace(alice)
			}
		}
	}()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4000; j++ {
				name, ok := users.Authenticate(r)
				if !ok || (name != "alice" && name != "bob") {
					t.Errorf("authentication observed partial snapshot: (%q, %v)", name, ok)
					return
				}
			}
		}()
	}
	wg.Wait()
}
