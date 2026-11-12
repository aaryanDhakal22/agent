package terminal

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// The call center's events (contract/agent.md §2.10ab "Events to main"):
// while main asks for them (command terminal.events.want {seconds}, sent
// again every minute while a call-center browser listens), every event the
// Pi publishes on /api/events goes to main as the report terminal.events
// {events: [{event, data, at}]}: batched every 200 ms, at most 100 per
// report, dropped (never queued) when main is unreachable - the app
// refetches its lists on reconnect.

// Messages (mirror internal/rpc's constants).
const (
	MsgTerminalEventsWant = "terminal.events.want"
	MsgTerminalEvents     = "terminal.events"
)

// Events forwarding limits.
const (
	EventsWantMaxSeconds = 300
	eventsBatchEvery     = 200 * time.Millisecond
	eventsBatchMax       = 100
	eventsBacklogMax     = 1000 // waiting for the next report; older ones drop
	eventsSendTimeout    = 5 * time.Second
)

// RelayEvent is one Pi event as main gets it.
type RelayEvent struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
	At    time.Time       `json:"at"`
}

// EventForwarder sends the Pi's events to main while main wants them.
type EventForwarder struct {
	// subscribe taps /api/events' broker: the events (the channel closes
	// when the broker dropped a slow reader) and its cancel.
	subscribe func() (<-chan RelayEvent, func())
	report    func(ctx context.Context, msgType string, payload any) error
	logger    zerolog.Logger
	now       func() time.Time
	every     time.Duration

	mu      sync.Mutex
	until   time.Time
	running bool
}

// NewEventForwarder builds the forwarder; HandleWant starts it.
func NewEventForwarder(subscribe func() (<-chan RelayEvent, func()), report func(ctx context.Context, msgType string, payload any) error,
	logger zerolog.Logger) *EventForwarder {
	return &EventForwarder{subscribe: subscribe, report: report, logger: logger.With().Str("module", "callcenter-events").Logger(),
		now: time.Now, every: eventsBatchEvery}
}

// HandleWant is main's `terminal.events.want {seconds}` command: forward
// events for that long from now (1-300 s; a later command moves the end).
// No reply.
func (f *EventForwarder) HandleWant(ctx context.Context, payload json.RawMessage) (any, error) {
	var p struct {
		Seconds int `json:"seconds"`
	}
	_ = json.Unmarshal(payload, &p)
	if p.Seconds <= 0 {
		f.mu.Lock()
		f.until = time.Time{} // main stopped listening
		f.mu.Unlock()
		return nil, nil
	}
	secs := min(p.Seconds, EventsWantMaxSeconds)
	f.mu.Lock()
	f.until = f.now().Add(time.Duration(secs) * time.Second)
	start := !f.running
	f.running = true
	f.mu.Unlock()
	if start {
		go f.run(context.WithoutCancel(ctx))
	}
	return nil, nil
}

// Wanted: main is listening now.
func (f *EventForwarder) Wanted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now().Before(f.until)
}

// run forwards until main's want runs out.
func (f *EventForwarder) run(ctx context.Context) {
	defer func() {
		if v := recover(); v != nil {
			f.logger.Error().Interface("panic", v).Msg("event forwarder panicked")
			f.mu.Lock()
			f.running = false
			f.mu.Unlock()
		}
	}()
	ch, cancel := f.subscribe()
	defer func() { cancel() }()
	t := time.NewTicker(f.every)
	defer t.Stop()
	var pending []RelayEvent
	for {
		select {
		case ev, ok := <-ch:
			if !ok { // the broker dropped us (slow): tap again, what was missed is gone
				cancel()
				pending = nil
				ch, cancel = f.subscribe()
				continue
			}
			pending = append(pending, ev)
			if len(pending) > eventsBacklogMax {
				pending = pending[len(pending)-eventsBacklogMax:]
			}
		case <-t.C:
			f.mu.Lock()
			if !f.now().Before(f.until) {
				f.running = false
				f.mu.Unlock()
				return
			}
			f.mu.Unlock()
			if len(pending) == 0 {
				continue
			}
			n := min(len(pending), eventsBatchMax)
			batch := pending[:n:n]
			pending = pending[n:]
			sctx, done := context.WithTimeout(ctx, eventsSendTimeout)
			err := f.report(sctx, MsgTerminalEvents, map[string]any{"events": batch})
			done()
			if err != nil { // main unreachable: dropped, never queued
				f.logger.Debug().Err(err).Int("events", len(batch)+len(pending)).Msg("terminal.events not sent: dropped")
				pending = nil
			}
		}
	}
}
