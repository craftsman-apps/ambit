package console

import (
	"sync"
	"time"
)

// SessionStore holds the sessions the console has issued. A cookie is valid only while its
// session ID is here, so sign-out and offboarding end a session even if the cookie was copied.
// ponytail: in memory, one console replica; a restart signs everyone out. Move to a shared
// store (PostgreSQL arrives in step 4) before running more than one replica.
type SessionStore struct {
	mu   sync.Mutex
	byID map[string]storedSession
}

type storedSession struct {
	email string
	exp   int64
}

func NewSessionStore() *SessionStore { return &SessionStore{byID: map[string]storedSession{}} }

func (s *SessionStore) Add(id, email string, exp int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	for k, v := range s.byID { // drop expired entries so the map cannot grow without bound
		if v.exp < now {
			delete(s.byID, k)
		}
	}
	s.byID[id] = storedSession{email: email, exp: exp}
}

func (s *SessionStore) Valid(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.byID[id]
	return ok && v.exp >= time.Now().Unix()
}

func (s *SessionStore) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
}

// RevokeUser ends every session of one person; offboarding (step 6) calls this.
func (s *SessionStore) RevokeUser(email string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, v := range s.byID {
		if v.email == email {
			delete(s.byID, k)
			n++
		}
	}
	return n
}

func (s *SessionStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	n := 0
	for _, v := range s.byID {
		if v.exp >= now {
			n++
		}
	}
	return n
}
