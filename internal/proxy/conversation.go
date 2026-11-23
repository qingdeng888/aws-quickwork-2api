package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// conversation keeps the upstream thread used for one client conversation and
// the client messages already forwarded to it.
type conversation struct {
	// sem is a one-slot semaphore so waiting for the conversation can be
	// abandoned when the request context ends.
	sem chan struct{}
	// refs counts requests holding or waiting on this entry; guarded by the
	// store mutex. Entries with refs > 0 are never evicted.
	refs         int
	threadID     string
	accountID    int64
	messages     []Message
	systemPrompt string
	lastUsed     atomic.Int64
}

func (c *conversation) touch() {
	c.lastUsed.Store(time.Now().UnixNano())
}

func (c *conversation) idle() time.Duration {
	n := c.lastUsed.Load()
	if n == 0 {
		return 0
	}
	return time.Since(time.Unix(0, n))
}

func (c *conversation) reset() {
	c.threadID = ""
	c.accountID = 0
	c.messages = nil
	c.systemPrompt = ""
}

// delta returns the messages added since the last forwarded turn. A false
// result means the history was edited or there is nothing new, so the caller
// must start a fresh upstream session with the full transcript.
func (c *conversation) delta(msgs []Message) ([]Message, bool) {
	if c.threadID == "" || len(c.messages) == 0 {
		return nil, false
	}
	if len(msgs) <= len(c.messages) {
		return nil, false
	}
	for i := range c.messages {
		if !reflect.DeepEqual(c.messages[i], msgs[i]) {
			return nil, false
		}
	}
	return msgs[len(c.messages):], true
}

type conversationStore struct {
	mu      sync.Mutex
	entries map[string]*conversation
	ttl     time.Duration
	max     int
}

func newConversationStore(ttl time.Duration, max int) *conversationStore {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	if max < 1 {
		max = 256
	}
	return &conversationStore{entries: make(map[string]*conversation), ttl: ttl, max: max}
}

// acquire locks one conversation for the duration of a request. Expired and
// least-recently-used conversations are dropped first. The store lock is
// released before waiting on a conversation so one busy chat cannot stall
// unrelated conversations.
func (s *conversationStore) acquire(ctx context.Context, key string) (*conversation, error) {
	s.mu.Lock()
	for k, c := range s.entries {
		if c.refs == 0 && c.idle() > s.ttl {
			delete(s.entries, k)
		}
	}
	if len(s.entries) >= s.max {
		var oldestKey string
		var oldest time.Time
		for k, c := range s.entries {
			if c.refs > 0 {
				continue
			}
			used := time.Unix(0, c.lastUsed.Load())
			if oldestKey == "" || used.Before(oldest) {
				oldestKey, oldest = k, used
			}
		}
		if oldestKey != "" {
			delete(s.entries, oldestKey)
		}
	}
	c := s.entries[key]
	if c == nil {
		c = &conversation{sem: make(chan struct{}, 1)}
		s.entries[key] = c
	}
	c.refs++
	s.mu.Unlock()

	select {
	case c.sem <- struct{}{}:
		c.touch()
		return c, nil
	case <-ctx.Done():
		s.mu.Lock()
		c.refs--
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (s *conversationStore) release(c *conversation) {
	c.touch()
	<-c.sem
	s.mu.Lock()
	c.refs--
	s.mu.Unlock()
}

// conversationKey identifies one OpenAI conversation. A client-provided
// X-Conversation-Id or X-Thread-Id wins; otherwise the full system prompt
// (including tool definitions) plus the first user message is used so the key
// stays stable while the transcript grows.
func conversationKey(r *http.Request, req *ChatRequest, fullSystemPrompt string) string {
	for _, header := range []string{"X-Conversation-Id", "X-Thread-Id"} {
		if id := strings.TrimSpace(r.Header.Get(header)); id != "" {
			// Scope client ids to the caller's key so different keys can never
			// share an upstream session by reusing the same id.
			sum := sha256.Sum256([]byte(tokenFromRequest(r)))
			return "h:" + hex.EncodeToString(sum[:])[:16] + ":" + id
		}
	}
	h := sha256.New()
	h.Write([]byte(r.Header.Get("Authorization")))
	h.Write([]byte{0})
	h.Write([]byte(r.Header.Get("X-Api-Key")))
	h.Write([]byte{0})
	h.Write([]byte(fullSystemPrompt))
	for _, m := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "system" || role == "developer" {
			continue
		}
		h.Write([]byte{0})
		h.Write([]byte(role))
		h.Write([]byte{0})
		h.Write([]byte(contentText(m.Content)))
		return "c:" + hex.EncodeToString(h.Sum(nil))
	}
	for _, m := range req.Messages {
		h.Write([]byte{0})
		h.Write([]byte(m.Role))
		h.Write([]byte(contentText(m.Content)))
	}
	return "c:" + hex.EncodeToString(h.Sum(nil))
}
