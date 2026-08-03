// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package etcd_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/conformance"
	"github.com/cosi-project/runtime/pkg/state/impl/store"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/goleak"

	"github.com/cosi-project/state-etcd/pkg/state/impl/etcd"
	"github.com/cosi-project/state-etcd/pkg/util/testhelpers"
)

// bookmarkRevision decodes the etcd revision a bookmark stands for.
//
// Bookmarks are the etcd revision in big-endian form, which makes them comparable across resource
// kinds - that is what lets these tests assert the delivery order directly.
func bookmarkRevision(t *testing.T, bookmark state.Bookmark) int64 {
	t.Helper()

	require.Len(t, bookmark, 8)

	return int64(binary.BigEndian.Uint64(bookmark))
}

// TestWatchKindAggregatedCrossKindOrdering verifies that watches of different resource kinds
// feeding a single channel deliver their events in the etcd revision order.
//
// Without a shared watcher each watch gets its own etcd watcher, and the etcd client library
// serves each of them on its own goroutine, so a resource created earlier can be observed after
// one created later.
func TestWatchKindAggregatedCrossKindOrdering(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	const (
		namespaces = 4
		writes     = 200
	)

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()

		// all the watches feed one channel, the way the COSI controller runtime consumes them
		watchCh := make(chan []state.Event, writes)

		for i := range namespaces {
			kind := conformance.NewPathResource(namespaceName(i), "").Metadata()

			require.NoError(t, s.WatchKindAggregated(ctx, kind, watchCh, state.WithBootstrapBookmark(true)))
		}

		// drain the bootstrap bookmarks before starting to write, so that the assertions below
		// only cover the live event stream
		for range namespaces {
			events := receiveBatch(ctx, t, watchCh)

			require.Len(t, events, 1)
			require.Equal(t, state.Noop, events[0].Type)
		}

		// a single writer makes the expected order deterministic: resources are created strictly
		// one after another, round-robin across the kinds
		expected := make([]string, 0, writes)

		for i := range writes {
			res := conformance.NewPathResource(namespaceName(i%namespaces), fmt.Sprintf("path-%d", i))

			require.NoError(t, s.Create(ctx, res))

			expected = append(expected, resource.String(res))
		}

		var (
			observed     []string
			lastRevision int64
		)

		for len(observed) < writes {
			for _, event := range receiveBatch(ctx, t, watchCh) {
				require.Equal(t, state.Created, event.Type, "unexpected event type")

				revision := bookmarkRevision(t, event.Bookmark)

				require.GreaterOrEqualf(t, revision, lastRevision,
					"event %d (%s) went back in revision: %d after %d",
					len(observed), resource.String(event.Resource), revision, lastRevision)

				lastRevision = revision

				observed = append(observed, resource.String(event.Resource))
			}
		}

		require.Equal(t, expected, observed, "events were not delivered in the order they were created")
	}, etcd.WithSharedWatch())
}

// TestWatchKindAggregatedOrderingWithBootstrap verifies that a watch registered while other
// watches are already running bootstraps at a revision consistent with the shared event stream.
func TestWatchKindAggregatedOrderingWithBootstrap(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()

		// populate the kind which will be bootstrapped later
		const bootstrapped = 5

		for i := range bootstrapped {
			require.NoError(t, s.Create(ctx, conformance.NewPathResource(namespaceName(1), fmt.Sprintf("pre-%d", i))))
		}

		watchCh := make(chan []state.Event, 128)

		require.NoError(t, s.WatchKindAggregated(ctx,
			conformance.NewPathResource(namespaceName(0), "").Metadata(), watchCh, state.WithBootstrapBookmark(true)))

		events := receiveBatch(ctx, t, watchCh)
		require.Len(t, events, 1)
		require.Equal(t, state.Noop, events[0].Type)

		// drive some traffic on the first kind, then register the second one with bootstrap
		for i := range 10 {
			require.NoError(t, s.Create(ctx, conformance.NewPathResource(namespaceName(0), fmt.Sprintf("live-%d", i))))
		}

		require.NoError(t, s.WatchKindAggregated(ctx,
			conformance.NewPathResource(namespaceName(1), "").Metadata(), watchCh, state.WithBootstrapContents(true)))

		var (
			created      int
			bootstrapSet int
			sawMarker    bool
		)

		for !sawMarker {
			for _, event := range receiveBatch(ctx, t, watchCh) {
				switch event.Type { //nolint:exhaustive
				case state.Created:
					if event.Resource.Metadata().Namespace() == namespaceName(1) {
						bootstrapSet++
					} else {
						created++
					}
				case state.Bootstrapped:
					// the bootstrap snapshot has to contain everything written before the watch
					// was established, regardless of where the shared watcher was positioned
					require.Equal(t, bootstrapped, bootstrapSet, "bootstrap snapshot is incomplete")

					sawMarker = true
				default:
					require.Failf(t, "unexpected event", "type %v", event.Type)
				}
			}
		}

		require.Equal(t, 10, created, "live events of the other kind were lost")
	}, etcd.WithSharedWatch())
}

// TestWatchSharedSingleResourceOrdering verifies single-resource watches take part in the same
// ordering as kind watches.
func TestWatchSharedSingleResourceOrdering(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()

		watched := conformance.NewPathResource(namespaceName(0), "watched")

		watchCh := make(chan state.Event, 128)

		require.NoError(t, s.Watch(ctx, watched.Metadata(), watchCh))
		require.NoError(t, s.WatchKind(ctx, conformance.NewPathResource(namespaceName(1), "").Metadata(), watchCh))

		// the initial event of the single-resource watch
		require.Equal(t, state.Destroyed, receiveEvent(ctx, t, watchCh).Type)

		const writes = 40

		expected := make([]string, 0, writes)

		for i := range writes {
			var res resource.Resource = conformance.NewPathResource(namespaceName(1), fmt.Sprintf("path-%d", i))

			// interleave the single-resource key with the other kind
			if i%4 == 0 {
				res = watched
			}

			if res == watched {
				if i > 0 {
					require.NoError(t, s.Destroy(ctx, watched.Metadata()))
				}

				require.NoError(t, s.Create(ctx, watched))
			} else {
				require.NoError(t, s.Create(ctx, res))
			}

			expected = append(expected, resource.String(res))
		}

		var lastRevision int64

		for range expected {
			event := receiveEvent(ctx, t, watchCh)

			revision := bookmarkRevision(t, event.Bookmark)

			require.GreaterOrEqual(t, revision, lastRevision, "event went back in revision")

			lastRevision = revision
		}
	}, etcd.WithSharedWatch())
}

// TestWatchSharedRestart verifies the shared watcher is released when the last watch goes away and
// is established again for a new one.
func TestWatchSharedRestart(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()

		for round := range 3 {
			roundCtx, roundCancel := context.WithCancel(ctx)

			watchCh := make(chan []state.Event, 16)

			require.NoError(t, s.WatchKindAggregated(roundCtx,
				conformance.NewPathResource(namespaceName(0), "").Metadata(), watchCh, state.WithBootstrapBookmark(true)))

			events := receiveBatch(roundCtx, t, watchCh)
			require.Len(t, events, 1)
			require.Equal(t, state.Noop, events[0].Type)

			require.NoError(t, s.Create(ctx, conformance.NewPathResource(namespaceName(0), fmt.Sprintf("round-%d", round))))

			events = receiveBatch(roundCtx, t, watchCh)
			require.Len(t, events, 1)
			require.Equal(t, state.Created, events[0].Type)

			roundCancel()
		}
	}, etcd.WithSharedWatch())
}

// countingClient counts the etcd watchers established through it.
type countingClient struct {
	etcd.Client

	watches atomic.Int64
}

func (c *countingClient) Watch(ctx context.Context, key string, opts ...clientv3.OpOption) clientv3.WatchChan {
	c.watches.Add(1)

	return c.Client.Watch(ctx, key, opts...)
}

// TestWatchSharedSingleEtcdWatcher verifies that all the watches of a State are served by one etcd
// watcher, rather than one per watch call.
func TestWatchSharedSingleEtcdWatcher(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	testhelpers.WithEtcd(t, func(cli *clientv3.Client) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()

		counting := &countingClient{Client: cli}

		s := state.WrapCore(etcd.NewState(counting, store.ProtobufMarshaler{},
			etcd.WithSalt([]byte("test123")), etcd.WithSharedWatch()))

		watchCh := make(chan state.Event, 256)

		for i := range 20 {
			require.NoError(t, s.Watch(ctx, conformance.NewPathResource(namespaceName(0), fmt.Sprintf("res-%d", i)).Metadata(), watchCh))
		}

		for i := range 5 {
			require.NoError(t, s.WatchKind(ctx, conformance.NewPathResource(namespaceName(i), "").Metadata(), watchCh))
		}

		// make sure the watches are actually live before counting
		require.NoError(t, s.Create(ctx, conformance.NewPathResource(namespaceName(0), "res-0")))
		require.Equal(t, state.Destroyed, receiveEvent(ctx, t, watchCh).Type)

		require.Equal(t, int64(1), counting.watches.Load(), "expected a single etcd watcher for 25 watches")
	})
}

func namespaceName(i int) string {
	return fmt.Sprintf("ns-%d", i)
}

func receiveBatch(ctx context.Context, t *testing.T, ch <-chan []state.Event) []state.Event {
	t.Helper()

	select {
	case events := <-ch:
		return events
	case <-ctx.Done():
		t.Fatal("timeout waiting for events")

		return nil
	}
}

func receiveEvent(ctx context.Context, t *testing.T, ch <-chan state.Event) state.Event {
	t.Helper()

	select {
	case event := <-ch:
		return event
	case <-ctx.Done():
		t.Fatal("timeout waiting for event")

		return state.Event{}
	}
}
