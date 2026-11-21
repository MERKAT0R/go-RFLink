/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"context"
	"sync"
)

const serialChannelBuffer = 64

// serialFrame is one gateway line plus the port that read it.
type serialFrame struct {
	Device string
	Line   string
}

func newSerialChannel() chan serialFrame {
	return make(chan serialFrame, serialChannelBuffer)
}

// serialIngress gives each gateway its own queue. A noisy port fills and drops
// only its own buffer. The publisher still receives one fair stream.
type serialIngress struct {
	mu       sync.Mutex
	byDevice map[string]chan serialFrame
	order    []string
	wake     chan struct{}
}

func newSerialIngress() *serialIngress {
	return &serialIngress{wake: make(chan struct{}, 1)}
}

type queueFill struct {
	Queued int
	Cap    int
}

// usage reports how full each gateway queue is. Ports that have not sent a
// line yet are absent; callers still show them as 0/capacity.
func (q *serialIngress) usage() map[string]queueFill {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make(map[string]queueFill, len(q.order))
	for _, device := range q.order {
		ch := q.byDevice[device]
		out[device] = queueFill{Queued: len(ch), Cap: cap(ch)}
	}
	return out
}

func (q *serialIngress) offer(frame serialFrame) bool {
	if q == nil {
		return false
	}
	ch := q.channel(frame.Device)
	select {
	case ch <- frame:
		select {
		case q.wake <- struct{}{}:
		default:
		}
		return true
	default:
		return false
	}
}

func (q *serialIngress) channel(device string) chan serialFrame {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.byDevice == nil {
		q.byDevice = map[string]chan serialFrame{}
	}
	ch := q.byDevice[device]
	if ch != nil {
		return ch
	}
	ch = make(chan serialFrame, serialChannelBuffer)
	q.byDevice[device] = ch
	q.order = append(q.order, device)
	return ch
}

// run copies one frame per gateway per pass into out until ctx is cancelled.
func (q *serialIngress) run(ctx context.Context, out chan<- serialFrame) {
	if q == nil {
		return
	}
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		}
		for q.flushOneRound(ctx, out) {
		}
	}
}

func (q *serialIngress) flushOneRound(ctx context.Context, out chan<- serialFrame) bool {
	q.mu.Lock()
	chans := make([]chan serialFrame, len(q.order))
	for i, device := range q.order {
		chans[i] = q.byDevice[device]
	}
	q.mu.Unlock()
	progressed := false
	for _, ch := range chans {
		if ctx.Err() != nil {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case frame := <-ch:
			if !q.forwardFrame(ctx, ch, out, frame) {
				return false
			}
			progressed = true
		default:
		}
	}
	return progressed
}

// forwardFrame delivers a frame that was already removed from ch.
// On shutdown it goes back onto that port's queue instead of being dropped.
func (q *serialIngress) forwardFrame(ctx context.Context, ch chan serialFrame, out chan<- serialFrame, frame serialFrame) bool {
	select {
	case out <- frame:
		return true
	default:
	}
	select {
	case out <- frame:
		return true
	case <-ctx.Done():
		select {
		case ch <- frame:
		default:
		}
		return false
	}
}
