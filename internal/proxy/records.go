package proxy

import (
	"sync"
	"time"
)

// ChatRecord is the summary of one finished chat request. It mirrors the
// completion log line so the management UI can show recent traffic without
// scraping stdout. No prompt or completion text is kept.
type ChatRecord struct {
	ID               string    `json:"id"`
	Time             time.Time `json:"time"`
	Model            string    `json:"model"`
	Status           string    `json:"status"`
	Account          string    `json:"account"`
	Attempts         int       `json:"attempts"`
	ElapsedMS        int64     `json:"elapsed_ms"`
	Stream           bool      `json:"stream"`
	PromptChars      int       `json:"prompt_chars"`
	SystemChars      int       `json:"system_chars"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	CacheReadTokens  int       `json:"cache_read_tokens"`
	CacheWriteTokens int       `json:"cache_write_tokens"`
	Finish           string    `json:"finish"`
	ToolCalls        int       `json:"tool_calls"`
	Error            string    `json:"error,omitempty"`
}

// ChatStats aggregates the records currently held in memory.
type ChatStats struct {
	Requests         int   `json:"requests"`
	OK               int   `json:"ok"`
	Failed           int   `json:"failed"`
	Empty            int   `json:"empty"`
	PromptTokens     int   `json:"prompt_tokens"`
	CompletionTokens int   `json:"completion_tokens"`
	TotalTokens      int   `json:"total_tokens"`
	CacheReadTokens  int   `json:"cache_read_tokens"`
	AvgElapsedMS     int64 `json:"avg_elapsed_ms"`
	Since            int64 `json:"since"`
	Last             int64 `json:"last"`
}

const chatRecordLimit = 500

// chatRecorder is a bounded, concurrency-safe ring of chat summaries.
type chatRecorder struct {
	mu    sync.Mutex
	items []ChatRecord
}

func (r *chatRecorder) add(rec ChatRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.items) >= chatRecordLimit {
		copy(r.items, r.items[len(r.items)-chatRecordLimit+1:])
		r.items = r.items[:chatRecordLimit-1]
	}
	r.items = append(r.items, rec)
}

// list returns up to limit records, newest first.
func (r *chatRecorder) list(limit int) []ChatRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 || limit > len(r.items) {
		limit = len(r.items)
	}
	out := make([]ChatRecord, 0, limit)
	for i := len(r.items) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, r.items[i])
	}
	return out
}

func (r *chatRecorder) stats() ChatStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	stats := ChatStats{Requests: len(r.items)}
	var elapsed int64
	for _, item := range r.items {
		switch item.Status {
		case "ok":
			stats.OK++
		case "empty":
			stats.Empty++
		default:
			stats.Failed++
		}
		stats.PromptTokens += item.PromptTokens
		stats.CompletionTokens += item.CompletionTokens
		stats.TotalTokens += item.TotalTokens
		stats.CacheReadTokens += item.CacheReadTokens
		elapsed += item.ElapsedMS
		if stats.Since == 0 || item.Time.Unix() < stats.Since {
			stats.Since = item.Time.Unix()
		}
		if item.Time.Unix() > stats.Last {
			stats.Last = item.Time.Unix()
		}
	}
	if stats.Requests > 0 {
		stats.AvgElapsedMS = elapsed / int64(stats.Requests)
	}
	return stats
}
