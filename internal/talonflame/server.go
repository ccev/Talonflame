package talonflame

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

type batch struct {
	messages []Webhook
	at       time.Time
}
type snapshotRequest struct{ reply chan []Group }

type Server struct {
	analyzer    *Analyzer
	queue       chan batch
	queries     chan snapshotRequest
	received    atomic.Uint64
	failed      atomic.Uint64
	processed   atomic.Uint64
	evidence    atomic.Uint64
	confirmed   atomic.Uint64
	conflicting atomic.Uint64
}

func NewServer(analyzer *Analyzer) *Server {
	server := &Server{analyzer: analyzer, queue: make(chan batch, 64), queries: make(chan snapshotRequest)}
	if analyzer != nil {
		server.recordState()
	}
	return server
}

func (s *Server) recordState() {
	s.evidence.Store(uint64(len(s.analyzer.evidence)))
	s.confirmed.Store(s.analyzer.knownGroups - s.analyzer.conflictingGroups)
	s.conflicting.Store(s.analyzer.conflictingGroups)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", s.webhook)
	mux.HandleFunc("/api/nests", s.nests)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.analyzer.db.PingContext(ctx); err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		w.WriteHeader(200)
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "talonflame_webhooks_received_total %d\ntalonflame_processing_errors_total %d\ntalonflame_webhook_queue_batches %d\n", s.received.Load(), s.failed.Load(), len(s.queue))
		fmt.Fprintf(w, "talonflame_messages_processed_total %d\ntalonflame_evidence_cached %d\ntalonflame_confirmed_nests %d\ntalonflame_conflicting_nests %d\n", s.processed.Load(), s.evidence.Load(), s.confirmed.Load(), s.conflicting.Load())
	})
	return mux
}

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST required", 405)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	decoder := json.NewDecoder(r.Body)
	var messages []Webhook
	if err := decoder.Decode(&messages); err != nil {
		http.Error(w, "invalid webhook batch", 400)
		return
	}
	if len(messages) > 10000 {
		http.Error(w, "batch exceeds 10000 messages", 413)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		http.Error(w, "expected one JSON array", 400)
		return
	}
	select {
	case s.queue <- batch{messages, time.Now()}:
		s.received.Add(uint64(len(messages)))
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "processing queue full", http.StatusServiceUnavailable)
	}
}

func (s *Server) nests(w http.ResponseWriter, r *http.Request) {
	reply := make(chan []Group, 1)
	select {
	case s.queries <- snapshotRequest{reply}:
	case <-r.Context().Done():
		return
	}
	select {
	case groups := <-reply:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(groups)
	case <-r.Context().Done():
	}
}

// Drain is called after the HTTP server has stopped accepting requests.
func (s *Server) Drain() { close(s.queue) }

func (s *Server) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	lastMaintenance := time.Now()
	process := func(item batch) {
		for _, message := range item.messages {
			if message.Type != "pokemon" {
				continue
			}
			if err := s.analyzer.Process(ctx, message.Message, item.at); err != nil {
				s.failed.Add(1)
				s.analyzer.logger.WithError(err).Warn("could not process Pokémon webhook")
			}
		}
		if err := s.analyzer.Evaluate(ctx, item.at); err != nil {
			s.failed.Add(1)
			s.analyzer.logger.WithError(err).Error("nest evaluation failed")
		}
		s.processed.Add(uint64(len(item.messages)))
		s.recordState()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case item, ok := <-s.queue:
			if !ok {
				return nil
			}
			process(item)
		case request := <-s.queries:
			request.reply <- s.analyzer.Groups()
		case now := <-ticker.C:
			// Keep the session holding the writer lease alive, and stop if the
			// session is lost rather than continuing without exclusivity.
			if err := s.analyzer.lock.PingContext(ctx); err != nil {
				return fmt.Errorf("writer lease connection lost: %w", err)
			}
			if err := s.analyzer.Repair(ctx, now); err != nil {
				s.failed.Add(1)
				s.analyzer.logger.WithError(err).Error("membership repair failed")
			}
			if now.Sub(lastMaintenance) >= time.Hour {
				if err := s.analyzer.Maintain(ctx, now); err != nil {
					s.failed.Add(1)
					s.analyzer.logger.WithError(err).Error("evidence maintenance failed")
				}
				lastMaintenance = now
			}
			s.recordState()
		}
	}
}
