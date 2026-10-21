package ssebroker

import "testing"

func TestSlowClientIsDisconnectedNotSilentlyDropped(t *testing.T) {
	b := New()
	ch, unsubscribe := b.Subscribe()
	for i := 0; i < cap(ch)+5; i++ {
		b.publishJSON("order.updated", map[string]int{"i": i})
	}
	if b.Subscribers() != 0 {
		t.Fatal("an overflowing client must be dropped")
	}
	n := 0
	for range ch { // closed after the buffered events
		n++
	}
	if n != cap(ch) {
		t.Fatalf("buffered events = %d", n)
	}
	unsubscribe() // must not panic on the already-closed channel
}

func TestFastClientGetsEverything(t *testing.T) {
	b := New()
	ch, unsubscribe := b.Subscribe()
	defer unsubscribe()
	b.publishJSON("drivers.updated", []int{})
	if ev := <-ch; ev.Type != "drivers.updated" {
		t.Fatalf("got %s", ev.Type)
	}
	if b.Subscribers() != 1 {
		t.Fatal("still subscribed")
	}
}
