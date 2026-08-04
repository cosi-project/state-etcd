// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package etcd

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"runtime/debug"
	"sort"
	"sync"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/channel"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// watchMux multiplexes a single etcd watcher established over the whole key prefix to any number
// of subscribers.
//
// A single etcd watcher combined with a single dispatching goroutine is what makes the delivery
// order well-defined: the etcd watch API guarantees that events are delivered in revision order
// within one watcher, and the dispatcher preserves that order while fanning the events out. With
// one etcd watcher per subscriber (the default mode) this does not hold, as the etcd client
// library serves every watcher on its own goroutine.
//
// The guarantee is scoped to a destination channel: events delivered to the same channel are in
// etcd revision order, no matter which watch produced them. Ordering cannot be global, because
// consumers routinely read several watch channels in a fixed order, and a dispatcher blocking on
// one of them would deadlock against a consumer waiting on another - the COSI conformance suite
// does exactly that.
type watchMux struct {
	st *State

	// index is copy-on-write: it is replaced (never mutated) under mu, so the dispatcher can grab
	// a consistent snapshot of it and use it without holding the lock.
	index *subsIndex

	// queues is keyed by the destination channel, so subscribers sharing a channel share a queue
	queues map[any]*outQueue

	// done is non-nil while a dispatcher goroutine exists (running or winding down), and is closed
	// once that goroutine is gone.
	done   chan struct{}
	cancel context.CancelFunc

	mu       sync.Mutex
	revision int64
	numSubs  int
	active   bool
}

// subsIndex maps etcd keys to the subscribers interested in them.
type subsIndex struct {
	// kinds is keyed by the kind prefix of the etcd key, serving WatchKind/WatchKindAggregated.
	kinds map[string][]*subscriber
	// exact is keyed by the full etcd key, serving single-resource Watch.
	exact map[string][]*subscriber
}

func newSubsIndex() *subsIndex {
	return &subsIndex{
		kinds: map[string][]*subscriber{},
		exact: map[string][]*subscriber{},
	}
}

func (idx *subsIndex) clone() *subsIndex {
	out := &subsIndex{
		kinds: make(map[string][]*subscriber, len(idx.kinds)),
		exact: make(map[string][]*subscriber, len(idx.exact)),
	}

	maps.Copy(out.kinds, idx.kinds)
	maps.Copy(out.exact, idx.exact)

	return out
}

func (idx *subsIndex) with(sub *subscriber) *subsIndex {
	out := idx.clone()

	target := out.kinds
	if sub.exact {
		target = out.exact
	}

	// copy the slice as well: the dispatcher may be iterating over the one currently published
	target[sub.key] = append(append([]*subscriber(nil), target[sub.key]...), sub)

	return out
}

func (idx *subsIndex) without(sub *subscriber) *subsIndex {
	out := idx.clone()

	target := out.kinds
	if sub.exact {
		target = out.exact
	}

	kept := make([]*subscriber, 0, len(target[sub.key]))

	for _, candidate := range target[sub.key] {
		if candidate != sub {
			kept = append(kept, candidate)
		}
	}

	if len(kept) == 0 {
		delete(target, sub.key)
	} else {
		target[sub.key] = kept
	}

	return out
}

// runKey returns the key under which the event for the given etcd key is dispatched.
//
// Events sharing a run key have the same set of interested subscribers, which is what lets the
// dispatcher batch a consecutive run of them into a single aggregated delivery without breaking
// the global ordering.
func (idx *subsIndex) runKey(key string) string {
	if _, ok := idx.exact[key]; ok {
		return key
	}

	return etcdKeyPrefixFromKey(key)
}

func (idx *subsIndex) subscribersFor(runKey string) []*subscriber {
	if subs, ok := idx.exact[runKey]; ok {
		// an exact run key still belongs to its kind, so both sets are interested
		return append(append([]*subscriber(nil), subs...), idx.kinds[etcdKeyPrefixFromKey(runKey)]...)
	}

	return idx.kinds[runKey]
}

func (idx *subsIndex) all() []*subscriber {
	var out []*subscriber

	for _, subs := range idx.kinds {
		out = append(out, subs...)
	}

	for _, subs := range idx.exact {
		out = append(out, subs...)
	}

	return out
}

// subscribe registers the subscriber with the shared watcher, starting it if needed.
//
// The subscriber has to be registered before its bootstrap snapshot is read, so that no event is
// lost in between: everything the snapshot does not cover is queued for the subscriber from this
// point on.
func (m *watchMux) subscribe(ctx context.Context, sub *subscriber) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// a previous dispatcher might still be winding down: it has to be gone before a new one is
	// started, otherwise two dispatchers would be delivering events concurrently
	for m.done != nil && !m.active {
		done := m.done

		m.mu.Unlock()
		<-done
		m.mu.Lock()
	}

	if !m.active {
		if err := m.startLocked(ctx); err != nil {
			return err
		}
	}

	sub.queue = m.queueForLocked(sub.destination())

	m.index = m.index.with(sub)
	m.numSubs++
	sub.registered = true

	return nil
}

// queueForLocked returns the queue serving the given destination channel, creating it if needed.
func (m *watchMux) queueForLocked(destination any) *outQueue {
	queue, ok := m.queues[destination]
	if !ok {
		queue = newOutQueue()
		m.queues[destination] = queue

		go queue.run()
	}

	queue.refs++

	return queue
}

// unsubscribe removes the subscriber, stopping the shared watcher once the last one is gone.
func (m *watchMux) unsubscribe(sub *subscriber) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.active || !sub.registered {
		return
	}

	sub.registered = false

	m.index = m.index.without(sub)
	m.numSubs--

	if sub.queue != nil {
		sub.queue.refs--

		if sub.queue.refs == 0 {
			delete(m.queues, sub.destination())

			close(sub.queue.done)
		}
	}

	if m.numSubs == 0 {
		// leave m.done set, so that a subscribe racing with the shutdown waits for the dispatcher
		// to be gone instead of attaching to it
		m.active = false

		m.cancel()
	}
}

// startLocked establishes the shared etcd watcher.
//
//nolint:contextcheck // the shared watcher runs on its own context, not on the caller's one
func (m *watchMux) startLocked(ctx context.Context) error {
	// fetch the revision to start the shared watcher at; a plain Get on the prefix key (without
	// WithPrefix) reads no data and only serves to sample the current store revision
	getResp, err := m.st.cli.Get(ctx, m.st.keyPrefix)
	if err != nil {
		return fmt.Errorf("etcd call failed on establishing shared watch: %w", err)
	}

	m.revision = getResp.Header.Revision
	m.index = newSubsIndex()
	m.queues = map[any]*outQueue{}
	m.numSubs = 0
	m.active = true

	// the shared watcher outlives any individual subscriber, so its context is deliberately not
	// derived from the context of whoever happened to establish it; it is canceled once the last
	// subscriber goes away
	watchCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	m.cancel = cancel
	m.done = done

	watchCh := m.st.cli.Watch(
		clientv3.WithRequireLeader(watchCtx),
		// the trailing slash matters: without it the range would also cover sibling prefixes
		// sharing this one as their string prefix (e.g. "/cosi" would also match "/cosi-other")
		m.st.keyPrefix+"/",
		clientv3.WithPrefix(),
		clientv3.WithPrevKV(),
		clientv3.WithRev(m.revision+1),
		// keeps m.revision (and therefore freshly handed out bookmarks) advancing on an idle
		// cluster, so they do not get compacted away
		clientv3.WithProgressNotify(),
	)

	go m.run(watchCtx, watchCh, done)

	return nil
}

func (m *watchMux) run(ctx context.Context, watchCh clientv3.WatchChan, done chan struct{}) {
	var watchErr error

	defer func() {
		// the dispatcher serves every watch of this State, so a panic here would take all of them
		// down without a trace; report it to the subscribers instead
		if p := recover(); p != nil {
			watchErr = fmt.Errorf("panic in shared watch dispatcher: %v\n%s", p, string(debug.Stack()))
		}

		m.stop(done, watchErr)

		// drain the watchCh, etcd Watch API guarantees that the channel is closed when the watcher is canceled
		for range watchCh { //nolint:revive
		}
	}()

	for watchResponse := range chanItems(ctx, watchCh) {
		if err := watchResponse.Err(); err != nil {
			switch {
			case errors.Is(err, rpctypes.ErrCompacted):
				err = ErrInvalidWatchBookmark(err)
			case errors.Is(err, rpctypes.ErrFutureRev):
				err = ErrInvalidWatchBookmark(err)
			}

			watchErr = err

			return
		}

		if watchResponse.Canceled {
			return
		}

		m.dispatch(&watchResponse)
	}
}

// stop tears the mux down, reporting the error which terminated the shared watcher (if any) to
// every remaining subscriber.
func (m *watchMux) stop(done chan struct{}, watchErr error) {
	m.mu.Lock()

	index := m.index
	queues := m.queues

	m.active = false
	m.index = nil
	m.queues = nil
	m.cancel = nil
	m.numSubs = 0
	m.done = nil

	m.mu.Unlock()

	if index != nil {
		for _, sub := range index.all() {
			// the shared watcher is gone, so every watch it served is over; report why unless it
			// simply ran out of subscribers
			sub.terminate(watchErr)
		}
	}

	// the subscribers are detached by now, so nothing new can be enqueued
	for _, queue := range queues {
		close(queue.done)
	}

	close(done)
}

func (m *watchMux) dispatch(resp *clientv3.WatchResponse) {
	m.mu.Lock()

	// every event carried by this response has a revision not greater than the response revision,
	// so bumping m.revision before the events are dispatched keeps the invariant a subscriber
	// registering concurrently relies on: everything up to the revision it is handed is readable
	// from its bootstrap snapshot
	if resp.Header.Revision > m.revision {
		m.revision = resp.Header.Revision
	}

	index := m.index

	m.mu.Unlock()

	if index == nil {
		return
	}

	events := resp.Events

	for i := 0; i < len(events); {
		runKey := index.runKey(etcdEventKey(events[i]))

		j := i + 1
		for j < len(events) && index.runKey(etcdEventKey(events[j])) == runKey {
			j++
		}

		m.dispatchRun(index, runKey, events[i:j])

		i = j
	}
}

// dispatchRun delivers a run of consecutive events which all share the same set of subscribers.
func (m *watchMux) dispatchRun(index *subsIndex, runKey string, etcdEvents []*clientv3.Event) {
	subs := index.subscribersFor(runKey)
	if len(subs) == 0 {
		// nobody is watching this kind: skip without paying for unmarshaling
		return
	}

	converted := make([]state.Event, 0, len(etcdEvents))

	for _, etcdEvent := range etcdEvents {
		event, err := m.st.convertEvent(etcdEvent)
		if err != nil {
			// the failure is confined to this kind, so only its subscribers are torn down and the
			// shared watcher keeps serving everyone else
			for _, sub := range subs {
				sub.terminate(err)
			}

			return
		}

		converted = append(converted, event)
	}

	for _, sub := range subs {
		sub.deliver(converted)
	}
}

func etcdEventKey(etcdEvent *clientv3.Event) string {
	if etcdEvent == nil || etcdEvent.Kv == nil {
		return ""
	}

	return string(etcdEvent.Kv.Key)
}

// subscriber is a single Watch/WatchKind/WatchKindAggregated call attached to the shared watcher.
type subscriber struct {
	ctx context.Context //nolint:containedctx // the subscriber lifetime is the caller's watch call lifetime
	st  *State
	mux *watchMux

	queue     *outQueue
	stopWatch func() bool

	// kind is set for kind subscribers, pointer for single-resource ones
	kind    resource.Kind
	pointer resource.Pointer

	singleCh chan<- state.Event
	aggCh    chan<- []state.Event

	// pending holds the events dispatched while the bootstrap snapshot is still being read
	pending [][]state.Event

	// key is the kind prefix, or the full etcd key for a single-resource watch
	key string

	options state.WatchKindOptions

	// anchor is the revision the bootstrap snapshot was read at; everything up to it is already
	// covered by the snapshot
	anchor int64

	mu         sync.Mutex
	exact      bool
	live       bool
	done       bool
	registered bool
}

// destination returns the key identifying the channel this subscriber writes to.
func (s *subscriber) destination() any {
	if s.aggCh != nil {
		return s.aggCh
	}

	return s.singleCh
}

func (s *subscriber) matches(res resource.Resource) bool {
	if res == nil {
		return false
	}

	return s.options.LabelQueries.Matches(*res.Metadata().Labels()) && s.options.IDQuery.Matches(*res.Metadata())
}

// filter narrows a dispatched run down to the events this subscriber is interested in.
//
// The returned slice is private to the subscriber: the events are copied by value, so rewriting an
// event type below cannot be observed by the other subscribers of the same run.
func (s *subscriber) filter(events []state.Event) []state.Event {
	out := make([]state.Event, 0, len(events))

	for _, event := range events {
		if s.exact {
			out = append(out, event)

			continue
		}

		switch event.Type {
		case state.Created, state.Destroyed:
			if !s.matches(event.Resource) {
				// skip the event
				continue
			}
		case state.Updated:
			oldMatches := s.matches(event.Old)
			newMatches := s.matches(event.Resource)

			switch {
			// transform the event if matching fact changes with the update
			case oldMatches && !newMatches:
				event.Type = state.Destroyed
				event.Old = nil
			case !oldMatches && newMatches:
				event.Type = state.Created
				event.Old = nil
			case newMatches && oldMatches:
				// passthrough the event
			default:
				// skip the event
				continue
			}
		case state.Errored, state.Bootstrapped, state.Noop:
			panic("should never be reached")
		}

		out = append(out, event)
	}

	return out
}

// deliver hands a dispatched run to the subscriber.
//
// It is called from the dispatcher goroutine only and never blocks: the batch is appended to the
// destination channel's queue, which is drained by the queue's own goroutine. Until the bootstrap
// snapshot has been spliced in, batches are held aside instead, so that they can be ordered
// against it.
func (s *subscriber) deliver(events []state.Event) {
	filtered := s.filter(events)
	if len(filtered) == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.done {
		return
	}

	if !s.live {
		s.pending = append(s.pending, filtered)

		return
	}

	// The shared watcher may still be lagging behind the revision the bootstrap snapshot was read
	// at, in which case it is about to replay events which are already reflected in that snapshot.
	// Delivering them would walk the resource back in time, so they are dropped here rather than
	// only while bootstrapping.
	if filtered = dropUpToRevision(filtered, s.anchor); len(filtered) == 0 {
		return
	}

	// enqueued under the subscriber lock, so that this cannot overtake the bootstrap splice
	s.queue.append(queueItem{sub: s, events: filtered})
}

// activate splices the bootstrap snapshot in front of everything dispatched since the subscriber
// was registered, and switches it over to direct delivery.
func (s *subscriber) activate(bootstrap []state.Event, revision int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.done {
		return
	}

	items := make([]queueItem, 0, len(s.pending)+1)

	if len(bootstrap) > 0 {
		items = append(items, queueItem{sub: s, events: bootstrap})
	}

	for _, batch := range s.pending {
		// events at or below the bootstrap revision are already part of the snapshot
		if batch = dropUpToRevision(batch, revision); len(batch) > 0 {
			items = append(items, queueItem{sub: s, events: batch})
		}
	}

	s.pending = nil
	s.anchor = revision
	s.live = true

	s.queue.append(items...)
}

// send pushes a batch to the caller's channel, reporting false if the caller is gone.
func (s *subscriber) send(events []state.Event) bool {
	if s.aggCh != nil {
		return channel.SendWithContext(s.ctx, s.aggCh, events)
	}

	for _, event := range events {
		if !channel.SendWithContext(s.ctx, s.singleCh, event) {
			return false
		}
	}

	return true
}

// terminate stops the subscriber, reporting the error to the caller unless it is already gone.
func (s *subscriber) terminate(err error) {
	s.mu.Lock()

	if s.done {
		s.mu.Unlock()

		return
	}

	s.done = true
	s.pending = nil
	queue := s.queue

	if err != nil && queue != nil {
		queue.append(queueItem{sub: s, events: []state.Event{{Type: state.Errored, Error: err}}})
	}

	s.mu.Unlock()

	if s.stopWatch != nil {
		s.stopWatch()
	}

	s.mux.unsubscribe(s)
}

func dropUpToRevision(events []state.Event, revision int64) []state.Event {
	out := events[:0]

	for _, event := range events {
		eventRevision, err := decodeBookmark(event.Bookmark)
		if err == nil && eventRevision <= revision {
			continue
		}

		out = append(out, event)
	}

	return out
}

// watchMuxed serves a watch from the shared watcher.
//
// It registers the subscriber, reads the bootstrap snapshot, and hands both over to the subscriber
// goroutine. The snapshot is read synchronously so that etcd failures are reported to the caller
// rather than as an Errored event, matching the behavior of the dedicated-watcher path.
func (st *State) watchMuxed(ctx context.Context, sub *subscriber, opName string) error {
	if err := st.mux.subscribe(ctx, sub); err != nil {
		return fmt.Errorf("failed to %s: %w", opName, err)
	}

	bootstrap, revision, err := sub.readBootstrap(ctx, opName)
	if err != nil {
		st.mux.unsubscribe(sub)

		return err
	}

	// release the shared watcher once the caller is done with this watch
	sub.stopWatch = context.AfterFunc(ctx, func() { sub.terminate(nil) })

	sub.activate(bootstrap, revision)

	return nil
}

// readBootstrap reads the initial contents of the watch, returning the revision they were read at.
//
// The read is always at the latest revision rather than at the revision the shared watcher is
// currently positioned at: callers expect a watch to bootstrap from the state as of the moment
// they asked for it, including their own writes, and the shared watcher may still be lagging
// behind. Everything the shared watcher has yet to deliver up to that revision is already part of
// the snapshot, and is dropped from the event stream by the subscriber (see dropUpToRevision).
func (s *subscriber) readBootstrap(ctx context.Context, opName string) ([]state.Event, int64, error) {
	if s.exact {
		return s.readBootstrapResource(ctx, opName)
	}

	switch {
	case s.options.BootstrapContents:
		return s.readBootstrapContents(ctx, opName)
	case s.options.BootstrapBookmark:
		revision, err := s.readRevision(ctx, opName)
		if err != nil {
			return nil, 0, err
		}

		return []state.Event{{
			Type:     state.Noop,
			Resource: resource.NewTombstone(resource.NewMetadata(s.kind.Namespace(), s.kind.Type(), "", resource.VersionUndefined)),
			Bookmark: encodeBookmark(revision),
		}}, revision, nil
	default:
		// no bootstrap block, but the revision is still needed: it is what makes the watch start
		// from the moment it was requested, rather than from wherever the shared watcher is
		revision, err := s.readRevision(ctx, opName)

		return nil, revision, err
	}
}

// readRevision samples the current store revision. The key is read as an exact key rather than as
// a prefix, so no data is transferred - only the response header matters.
func (s *subscriber) readRevision(ctx context.Context, opName string) (int64, error) {
	getResp, err := s.st.cli.Get(ctx, s.key)
	if err != nil {
		return 0, fmt.Errorf("etcd call failed on %s %q: %w", opName, s.kind, err)
	}

	return getResp.Header.Revision, nil
}

func (s *subscriber) readBootstrapContents(ctx context.Context, opName string) ([]state.Event, int64, error) {
	getResp, err := s.st.cli.Get(ctx, s.key, clientv3.WithPrefix())
	if err != nil {
		return nil, 0, fmt.Errorf("etcd call failed on %s %q: %w", opName, s.kind, err)
	}

	revision := getResp.Header.Revision

	var bootstrapList []resource.Resource

	for _, kv := range getResp.Kvs {
		res, err := s.st.unmarshalResource(kv)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to unmarshal on %s %q: %w", opName, s.kind, err)
		}

		if !s.matches(res) {
			continue
		}

		bootstrapList = append(bootstrapList, res)
	}

	sort.Slice(bootstrapList, func(i, j int) bool {
		return bootstrapList[i].Metadata().ID() < bootstrapList[j].Metadata().ID()
	})

	events := make([]state.Event, 0, len(bootstrapList)+1)

	for _, res := range bootstrapList {
		events = append(events, state.Event{
			Type:     state.Created,
			Resource: res,
		})
	}

	events = append(events, state.Event{
		Type:     state.Bootstrapped,
		Resource: resource.NewTombstone(resource.NewMetadata(s.kind.Namespace(), s.kind.Type(), "", resource.VersionUndefined)),
		Bookmark: encodeBookmark(revision),
	})

	return events, revision, nil
}

func (s *subscriber) readBootstrapResource(ctx context.Context, opName string) ([]state.Event, int64, error) {
	getResp, err := s.st.cli.Get(ctx, s.key)
	if err != nil {
		return nil, 0, fmt.Errorf("etcd call failed on %s %q: %w", opName, s.pointer, err)
	}

	revision := getResp.Header.Revision

	initialEvent := state.Event{
		Bookmark: encodeBookmark(revision),
	}

	if len(getResp.Kvs) > 0 {
		res, err := s.st.unmarshalResource(getResp.Kvs[0])
		if err != nil {
			return nil, 0, fmt.Errorf("failed to unmarshal on %s %q: %w", opName, s.pointer, err)
		}

		initialEvent.Resource = res
		initialEvent.Type = state.Created
	} else {
		initialEvent.Resource = resource.NewTombstone(
			resource.NewMetadata(s.pointer.Namespace(), s.pointer.Type(), s.pointer.ID(), resource.VersionUndefined),
		)
		initialEvent.Type = state.Destroyed
	}

	return []state.Event{initialEvent}, revision, nil
}
