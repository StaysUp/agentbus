package gateway

import (
	"bytes"
	"testing"

	"github.com/staysup/agentbus/body"
)

type emptySplit struct{}

func (emptySplit) Split([]byte) (body.Request, error) { return body.Request{}, nil }

// Empty bodies queued behind a blocked keeper stay under the entry cap, and
// the extras are dropped and counted as an oversized body is.
func TestBodyQueueBoundsEmptyBodies(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	first := true
	keep := body.KeeperFunc(func(body.Call) error {
		if first {
			first = false
			close(started)
			<-release
		}
		return nil
	})
	const entries = 64
	q := newBodyQueue(emptySplit{}, keep, 1<<20, entries)
	q.push(pendingBody{exchange: "x-0"})
	<-started // the keeper now holds the first body

	const sent = 100_000
	for i := 0; i < sent; i++ {
		q.push(pendingBody{exchange: "x-empty"})
	}
	q.mu.Lock()
	waiting, dropped, held := len(q.pending), q.dropped, q.bytes
	q.mu.Unlock()
	if waiting != entries || dropped != sent-entries {
		t.Errorf("waiting %d, dropped %d; want %d and %d", waiting, dropped, entries, sent-entries)
	}
	if want := (entries + 1) * bodyEntryOverhead; held != want {
		t.Errorf("bytes counted %d, want %d: empty bodies must count their overhead", held, want)
	}

	// An entry under the cap that is over the byte bound is dropped and counted
	// the same way.
	q2 := newBodyQueue(emptySplit{}, body.KeeperFunc(func(body.Call) error { <-release; return nil }), 4096, entries)
	q2.push(pendingBody{exchange: "x-big", data: bytes.Repeat([]byte("a"), 4096)})
	q2.mu.Lock()
	if q2.dropped != 1 || len(q2.pending) != 0 {
		t.Errorf("oversized: dropped %d, waiting %d", q2.dropped, len(q2.pending))
	}
	q2.mu.Unlock()

	close(release)
	q.close()
	q2.close()
}
