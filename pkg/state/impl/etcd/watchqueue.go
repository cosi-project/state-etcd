// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package etcd

import (
	"sync"

	"github.com/cosi-project/runtime/pkg/state"
)

// outQueue serializes the delivery to a single destination channel.
//
// Ordering is a property of the destination channel, not of the whole State: subscribers writing
// to the same channel are served by one queue, in the order the dispatcher enqueued them, which is
// the etcd revision order. Subscribers writing to different channels get separate queues and
// therefore cannot block one another - the dispatcher itself never blocks on a send.
type outQueue struct {
	notify chan struct{}
	done   chan struct{}

	items []queueItem

	mu   sync.Mutex
	refs int
}

type queueItem struct {
	sub    *subscriber
	events []state.Event
}

func newOutQueue() *outQueue {
	return &outQueue{
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
}

// append adds items to the queue. It never blocks.
func (q *outQueue) append(items ...queueItem) {
	if len(items) == 0 {
		return
	}

	q.mu.Lock()
	q.items = append(q.items, items...)
	q.mu.Unlock()

	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *outQueue) take() []queueItem {
	q.mu.Lock()
	defer q.mu.Unlock()

	items := q.items
	q.items = nil

	return items
}

// run delivers the queued batches to the destination channel until the queue is released.
func (q *outQueue) run() {
	for {
		items := q.take()

		for _, item := range items {
			// the send is bounded by the subscriber's own context, so a caller which went away
			// cannot wedge the queue for the subscribers sharing this channel
			if !item.sub.send(item.events) {
				item.sub.terminate(nil)
			}
		}

		if len(items) > 0 {
			continue
		}

		select {
		case <-q.notify:
		case <-q.done:
			// deliver whatever was enqueued just before the shutdown, notably the Errored events
			// reporting why the shared watcher went away
			for _, item := range q.take() {
				if !item.sub.send(item.events) {
					break
				}
			}

			return
		}
	}
}
