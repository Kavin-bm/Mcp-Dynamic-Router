package streamrag

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/kavinbm16/mcp-dynamic-router/router"
)

// Event is one ASR update. Final=false means the user may still be speaking.
type Event struct {
	Transcript string  `json:"transcript"`
	Context    string  `json:"context,omitempty"`
	Final      bool    `json:"final"`
	Confidence float64 `json:"confidence,omitempty"`
}

type Options struct {
	MinPartialCharacters int
	StableUpdates        int
	MinASRConfidence     float64
	PrefetchReadOnly     bool
}

func DefaultOptions() Options {
	return Options{MinPartialCharacters: 8, StableUpdates: 2, MinASRConfidence: 0.55, PrefetchReadOnly: true}
}

type Prediction struct {
	Triggered      bool               `json:"triggered"`
	Final          bool               `json:"final"`
	Stable         bool               `json:"stable"`
	StabilityCount int                `json:"stability_count"`
	Result         router.RouteResult `json:"result"`
	Reason         string             `json:"reason,omitempty"`
}

type Hooks struct {
	OnPrediction func(Prediction)
	// OnPrefetch may warm application data or downstream connections. It must
	// never perform a user-visible side effect.
	OnPrefetch func(context.Context, router.Tool) error
	OnCommit   func(Prediction)
	OnError    func(error)
}

// Session incrementally routes partial transcripts. A newer update cancels
// work for the older partial, which prevents stale ASR hypotheses from winning.
type Session struct {
	engine  *router.Router
	options Options
	hooks   Hooks

	mu             sync.Mutex
	generation     uint64
	cancel         context.CancelFunc
	closed         bool
	lastToolID     string
	stabilityCount int
	prefetched     map[string]struct{}
	wait           sync.WaitGroup
}

func New(engine *router.Router, options Options, hooks Hooks) *Session {
	defaults := DefaultOptions()
	if options.MinPartialCharacters <= 0 {
		options.MinPartialCharacters = defaults.MinPartialCharacters
	}
	if options.StableUpdates <= 0 {
		options.StableUpdates = defaults.StableUpdates
	}
	if options.MinASRConfidence <= 0 {
		options.MinASRConfidence = defaults.MinASRConfidence
	}
	return &Session{engine: engine, options: options, hooks: hooks, prefetched: make(map[string]struct{})}
}

// Submit starts routing asynchronously and returns immediately. It is the
// preferred API for realtime pipelines.
func (s *Session) Submit(parent context.Context, event Event) bool {
	if reason := s.ignoreReason(event); reason != "" {
		return false
	}
	ctx, generation, ok := s.begin(parent)
	if !ok {
		return false
	}
	s.wait.Add(1)
	go func() {
		defer s.wait.Done()
		prediction, err := s.route(ctx, generation, event)
		if err != nil {
			if err != errStaleGeneration && ctx.Err() == nil && s.hooks.OnError != nil {
				s.hooks.OnError(err)
			}
			return
		}
		s.publish(ctx, prediction)
	}()
	return true
}

// Update performs the same incremental routing synchronously. It is convenient
// for request/response transports such as the bundled HTTP sidecar.
func (s *Session) Update(parent context.Context, event Event) (Prediction, error) {
	if reason := s.ignoreReason(event); reason != "" {
		return Prediction{Triggered: false, Final: event.Final, Reason: reason}, nil
	}
	ctx, generation, ok := s.begin(parent)
	if !ok {
		return Prediction{}, fmt.Errorf("stream RAG session is closed")
	}
	prediction, err := s.route(ctx, generation, event)
	if err == errStaleGeneration {
		return Prediction{Triggered: false, Final: event.Final, Reason: "stale generation"}, nil
	}
	if err != nil {
		return Prediction{}, err
	}
	s.publish(ctx, prediction)
	return prediction, nil
}

func (s *Session) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		if s.cancel != nil {
			s.cancel()
		}
	}
	s.mu.Unlock()
	s.wait.Wait()
}

func (s *Session) begin(parent context.Context) (context.Context, uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, 0, false
	}
	if s.cancel != nil {
		s.cancel()
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.generation++
	return ctx, s.generation, true
}

var errStaleGeneration = fmt.Errorf("stale generation")

func (s *Session) route(ctx context.Context, generation uint64, event Event) (Prediction, error) {
	result, err := s.engine.Route(ctx, router.RouteRequest{Utterance: event.Transcript, Context: event.Context, Final: event.Final})
	if err != nil {
		return Prediction{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation {
		return Prediction{}, errStaleGeneration
	}
	toolID := ""
	if result.Decision == router.DecisionSelected && len(result.Candidates) > 0 {
		toolID = result.Candidates[0].Tool.ID
	}
	if toolID != "" && toolID == s.lastToolID {
		s.stabilityCount++
	} else if toolID != "" {
		s.lastToolID, s.stabilityCount = toolID, 1
	} else {
		s.lastToolID, s.stabilityCount = "", 0
	}
	return Prediction{Triggered: true, Final: event.Final, Stable: s.stabilityCount >= s.options.StableUpdates, StabilityCount: s.stabilityCount, Result: result}, nil
}

func (s *Session) publish(ctx context.Context, prediction Prediction) {
	if s.hooks.OnPrediction != nil {
		s.hooks.OnPrediction(prediction)
	}
	if prediction.Stable && !prediction.Final && s.options.PrefetchReadOnly && len(prediction.Result.Candidates) > 0 {
		tool := prediction.Result.Candidates[0].Tool
		if tool.ReadOnly && s.markPrefetched(tool.ID) && s.hooks.OnPrefetch != nil {
			if err := s.hooks.OnPrefetch(ctx, tool); err != nil && s.hooks.OnError != nil {
				s.hooks.OnError(fmt.Errorf("prefetch %s: %w", tool.ID, err))
			}
		}
	}
	if prediction.Final && s.hooks.OnCommit != nil {
		s.hooks.OnCommit(prediction)
	}
}

func (s *Session) markPrefetched(toolID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.prefetched[toolID]; exists {
		return false
	}
	s.prefetched[toolID] = struct{}{}
	return true
}

func (s *Session) ignoreReason(event Event) string {
	if strings.TrimSpace(event.Transcript) == "" {
		return "empty transcript"
	}
	if event.Final {
		return ""
	}
	if len([]rune(strings.TrimSpace(event.Transcript))) < s.options.MinPartialCharacters {
		return "partial transcript is too short"
	}
	if event.Confidence > 0 && event.Confidence < s.options.MinASRConfidence {
		return "ASR confidence is below the partial-routing threshold"
	}
	return ""
}
