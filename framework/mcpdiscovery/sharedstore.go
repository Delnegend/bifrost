package mcpdiscovery

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
)

// sharedWriteTimeout bounds one write of a client's tools to its row.
const sharedWriteTimeout = 10 * time.Second

var _ schemas.MCPDiscoveryStore = (*SharedStore)(nil)

// SharedStore keeps the discoveries of MCP clients without
// per_user_discovery: one per client, served to every caller from memory,
// which is the source of truth, and written to the client's row so a restart
// and the admin API find them.
//
// The manager calls it while holding its own lock, so nothing here waits on
// the database. SetDiscovery records the discovery and hands the write to a
// background writer, one per client, which writes the latest discovery it has
// been handed and skips one whose content the row already holds. A discovery
// with no DiscoveredAt was not produced by an upstream tools/list (the
// manager restored it from the row), so it is taken as what the row holds:
// it is not written, and a write handed over before it is dropped.
//
// All methods are safe on a nil receiver: reads miss and writes are refused.
type SharedStore struct {
	configStore configstore.ConfigStore
	logger      schemas.Logger

	mu          sync.RWMutex
	discoveries map[string]*schemas.MCPDiscovery // by client ID
	writes      map[string]*sharedWrite          // by client ID

	// writeGate, when set, is asked before every write; see SetWriteGate.
	writeGate atomic.Pointer[func() bool]
}

// sharedWrite is what the store knows about one client's row. seq orders the
// client's SetDiscovery calls, so a slow write never records itself over a
// newer restore.
type sharedWrite struct {
	seq         uint64
	pending     *schemas.MCPDiscovery // the latest discovery not yet written; nil when none
	pendingSeq  uint64
	running     bool                  // a writer is draining pending
	written     *schemas.MCPDiscovery // what the row holds: the last discovery written or restored
	writtenSeq  uint64
	writtenHash string // hash of written; "" until computed
}

// NewSharedStore returns a SharedStore writing through configStore. A nil
// configStore keeps discoveries in memory only.
func NewSharedStore(configStore configstore.ConfigStore, logger schemas.Logger) *SharedStore {
	if logger == nil {
		logger = bifrost.NewDefaultLogger(schemas.LogLevelInfo)
	}
	return &SharedStore{
		configStore: configStore,
		logger:      logger,
		discoveries: make(map[string]*schemas.MCPDiscovery),
		writes:      make(map[string]*sharedWrite),
	}
}

// GetDiscovery returns client's discovery; ctx is ignored.
func (s *SharedStore) GetDiscovery(_ context.Context, client *schemas.MCPClientConfig) *schemas.MCPDiscovery {
	if s == nil || client == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.discoveries[client.ID]
}

// SetDiscovery replaces client's discovery and, unless it was restored,
// queues it to be written to the client's row; ctx is ignored. It never waits
// on the database.
func (s *SharedStore) SetDiscovery(_ context.Context, client *schemas.MCPClientConfig, discovery *schemas.MCPDiscovery) error {
	if s == nil {
		return fmt.Errorf("shared discovery store is not configured")
	}
	if client == nil || discovery == nil {
		return fmt.Errorf("shared discovery requires a client and a discovery")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discoveries[client.ID] = discovery
	if s.configStore == nil {
		return nil
	}
	w := s.writes[client.ID]
	if w == nil {
		w = &sharedWrite{}
		s.writes[client.ID] = w
	}
	w.seq++
	if discovery.DiscoveredAt.IsZero() {
		w.written, w.writtenSeq, w.writtenHash = discovery, w.seq, ""
		return nil
	}
	w.pending, w.pendingSeq = discovery, w.seq
	if !w.running {
		w.running = true
		go s.drain(client.ID, w)
	}
	return nil
}

// DeleteDiscovery forgets client's discovery; ctx is ignored. The row is left
// as it is: a disabled client is restored from it when re-enabled, and a
// removed client's row goes with the client. A write already handed to the
// writer still lands.
func (s *SharedStore) DeleteDiscovery(_ context.Context, client *schemas.MCPClientConfig) error {
	if s == nil || client == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.discoveries, client.ID)
	if w := s.writes[client.ID]; w != nil && !w.running {
		delete(s.writes, client.ID)
	}
	return nil
}

// SetWriteGate registers allow, asked before every write to a client's row; a
// write it refuses is skipped, and the client's next discovery tries again.
// Every process discovers the same tools for a shared client, so a deployment
// running several processes over one database can leave the writes to one of
// them. nil clears it.
func (s *SharedStore) SetWriteGate(allow func() bool) {
	if s == nil {
		return
	}
	if allow == nil {
		s.writeGate.Store(nil)
		return
	}
	s.writeGate.Store(&allow)
}

// drain writes clientID's pending discovery until there is none. Hashing and
// the write itself run without s.mu, so neither holds up the manager.
func (s *SharedStore) drain(clientID string, w *sharedWrite) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("panic while persisting the discovered tools of MCP client %s: %v", clientID, r)
			s.mu.Lock()
			w.running = false
			s.mu.Unlock()
		}
	}()
	for {
		s.mu.Lock()
		discovery, seq := w.pending, w.pendingSeq
		w.pending = nil
		written, writtenHash := w.written, w.writtenHash
		if discovery == nil {
			w.running = false
			if _, ok := s.discoveries[clientID]; !ok && s.writes[clientID] == w {
				delete(s.writes, clientID)
			}
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		hash, err := discoveryHash(discovery.Tools, discovery.ToolNameMapping, discovery.Instructions)
		if err != nil {
			s.logger.Warn("failed to hash the discovered tools of MCP client %s: %v", clientID, err)
			continue
		}
		if writtenHash == "" && written != nil {
			if writtenHash, err = discoveryHash(written.Tools, written.ToolNameMapping, written.Instructions); err == nil {
				s.mu.Lock()
				if w.written == written {
					w.writtenHash = writtenHash
				}
				s.mu.Unlock()
			}
		}
		if hash == writtenHash || !s.mayWrite() {
			continue
		}
		s.mu.Lock()
		superseded := w.writtenSeq > seq // restored since it was handed over
		s.mu.Unlock()
		if superseded || s.write(clientID, discovery) != nil {
			continue
		}
		s.mu.Lock()
		if w.writtenSeq < seq {
			w.written, w.writtenSeq, w.writtenHash = discovery, seq, hash
		}
		s.mu.Unlock()
	}
}

// mayWrite asks the write gate, if one is set.
func (s *SharedStore) mayWrite() bool {
	allow := s.writeGate.Load()
	return allow == nil || (*allow)()
}

// write stores discovery on clientID's row. A client with no row (one that is
// not persisted, or was just removed) is not an error worth more than a debug
// line.
func (s *SharedStore) write(clientID string, discovery *schemas.MCPDiscovery) error {
	ctx, cancel := context.WithTimeout(context.Background(), sharedWriteTimeout)
	defer cancel()
	err := s.configStore.UpdateMCPClientTools(ctx, clientID, discovery.Tools, discovery.ToolNameMapping, discovery.Instructions)
	switch {
	case errors.Is(err, configstore.ErrNotFound):
		s.logger.Debug("MCP client %s has no row to persist its discovered tools to", clientID)
	case err != nil:
		s.logger.Error("failed to persist the discovered tools of MCP client %s: %v", clientID, err)
	}
	return err
}
