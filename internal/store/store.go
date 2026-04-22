package store

import (
	"sort"
	"sync"
	"time"

	"quiccpos/agent/internal/domain/order"
)

const ttl = 24 * time.Hour

type Entry struct {
	Order     order.OrderRequest `json:"order"`
	PrintedAt time.Time          `json:"printed_at"`
}

type Store struct {
	mu      sync.RWMutex
	entries map[int]Entry
}

func New() *Store {
	s := &Store{entries: make(map[int]Entry)}
	go s.runCleanup()
	return s
}

func (s *Store) Add(o order.OrderRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[o.OrderID] = Entry{Order: o, PrintedAt: time.Now()}
}

func (s *Store) Has(orderID int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.entries[orderID]
	return ok
}

func (s *Store) GetByID(orderID int) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[orderID]
	return e, ok
}

// List returns all entries sorted newest-first by PrintedAt.
func (s *Store) List() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].PrintedAt.After(out[j].PrintedAt)
	})
	return out
}

func (s *Store) runCleanup() {
	ticker := time.NewTicker(10 * time.Minute)
	for range ticker.C {
		cutoff := time.Now().Add(-ttl)
		s.mu.Lock()
		for id, e := range s.entries {
			if e.PrintedAt.Before(cutoff) {
				delete(s.entries, id)
			}
		}
		s.mu.Unlock()
	}
}
