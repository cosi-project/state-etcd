// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package etcd_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/conformance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestWatchKindWithBootstrap(t *testing.T) {
	for _, test := range []struct {
		name                      string
		destroyIsTheLastOperation bool
	}{
		{
			name:                      "put is last",
			destroyIsTheLastOperation: false,
		},
		{
			name:                      "delete is last",
			destroyIsTheLastOperation: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

			withEtcd(t, func(s state.State) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()

				for i := range 3 {
					require.NoError(t, s.Create(ctx, conformance.NewPathResource("default", fmt.Sprintf("path-%d", i))))
				}

				if test.destroyIsTheLastOperation {
					require.NoError(t, s.Create(ctx, conformance.NewPathResource("default", "path-3")))
					require.NoError(t, s.Destroy(ctx, conformance.NewPathResource("default", "path-3").Metadata()))
				}

				watchCh := make(chan state.Event)

				require.NoError(t, s.WatchKind(ctx, conformance.NewPathResource("default", "").Metadata(), watchCh, state.WithBootstrapContents(true)))

				for i := range 3 {
					select {
					case <-time.After(time.Second):
						t.Fatal("timeout waiting for event")
					case ev := <-watchCh:
						assert.Equal(t, state.Created, ev.Type)
						assert.Equal(t, fmt.Sprintf("path-%d", i), ev.Resource.Metadata().ID())
						assert.IsType(t, &conformance.PathResource{}, ev.Resource)
					}
				}

				select {
				case <-time.After(time.Second):
					t.Fatal("timeout waiting for event")
				case ev := <-watchCh:
					assert.Equal(t, state.Bootstrapped, ev.Type)
				}

				require.NoError(t, s.Destroy(ctx, conformance.NewPathResource("default", "path-0").Metadata()))

				select {
				case <-time.After(time.Second):
					t.Fatal("timeout waiting for event")
				case ev := <-watchCh:
					assert.Equal(t, state.Destroyed, ev.Type, "event %s %s", ev.Type, ev.Resource)
					assert.Equal(t, "path-0", ev.Resource.Metadata().ID())
					assert.IsType(t, &conformance.PathResource{}, ev.Resource)
				}

				newR, err := safe.StateUpdateWithConflicts(ctx, s, conformance.NewPathResource("default", "path-1").Metadata(), func(r *conformance.PathResource) error {
					r.Metadata().Finalizers().Add("foo")

					return nil
				})
				require.NoError(t, err)

				select {
				case <-time.After(time.Second):
					t.Fatal("timeout waiting for event")
				case ev := <-watchCh:
					assert.Equal(t, state.Updated, ev.Type, "event %s %s", ev.Type, ev.Resource)
					assert.Equal(t, "path-1", ev.Resource.Metadata().ID())
					assert.Equal(t, newR.Metadata().Finalizers(), ev.Resource.Metadata().Finalizers())
					assert.Equal(t, newR.Metadata().Version(), ev.Resource.Metadata().Version())
					assert.IsType(t, &conformance.PathResource{}, ev.Resource)
				}
			})
		})
	}
}

func TestWatchSpuriousEvents(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t, goleak.IgnoreCurrent()) })

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer cancel()

		const N = 100

		for i := range N {
			require.NoError(t, s.Create(ctx, conformance.NewPathResource("default", fmt.Sprintf("path-%d", i))))
		}

		watchCh := make(chan state.Event)

		for i := range N {
			require.NoError(t, s.Watch(ctx, conformance.NewPathResource("default", fmt.Sprintf("path-%d", i)).Metadata(), watchCh))
		}

		for range N {
			select {
			case <-time.After(time.Second):
				t.Fatal("timeout waiting for event")
			case ev := <-watchCh:
				assert.Equal(t, state.Created, ev.Type)
			}
		}

		for i := range N {
			require.NoError(t, s.Destroy(ctx, conformance.NewPathResource("default", fmt.Sprintf("path-%d", i)).Metadata()))
		}

		for range N {
			select {
			case <-time.After(time.Second):
				t.Fatal("timeout waiting for event")
			case ev := <-watchCh:
				assert.Equal(t, state.Destroyed, ev.Type, "ev: %v", ev.Resource)
			}
		}
	})
}

func TestWatchDeadCancel(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t, goleak.IgnoreCurrent()) })

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		const N = 50

		// the key to reproduce the bug is to create enough watchers (>25 seems to be enough),
		// and cancel all of them at the same time
		// after that trigger a couple of updates, and the next Watch will hang forever
		deadCtx, deadCancel := context.WithCancel(ctx)
		deadCh := make(chan state.Event)

		for range N {
			require.NoError(t, s.Watch(deadCtx, conformance.NewPathResource("default", "path-0").Metadata(), deadCh))
		}

		t.Log("dead cancel!")
		deadCancel()

		require.NoError(t, s.Create(ctx, conformance.NewPathResource("default", "path-0")))
		require.NoError(t, s.Destroy(ctx, conformance.NewPathResource("default", "path-0").Metadata()))

		watchCh := make(chan state.Event)

		// when the bug is triggered, s.Watch will hang forever
		//
		// the bug seems to be related to etcd client `*watcher` object getting into a deadlock
		// the bug doesn't reproduce if we use a real gRPC connection for the embedded etcd instead of "passthrough" mode
		require.NoError(t, s.Watch(ctx, conformance.NewPathResource("default", "path-0").Metadata(), watchCh))

		select {
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for event")
		case ev := <-watchCh:
			assert.Equal(t, state.Destroyed, ev.Type, "ev: %v", ev.Resource)
		}

		for range N {
			require.NoError(t, s.Create(ctx, conformance.NewPathResource("default", "path-0")))

			select {
			case <-time.After(time.Second):
				t.Fatal("timeout waiting for event")
			case ev := <-watchCh:
				assert.Equal(t, state.Created, ev.Type)
			}

			require.NoError(t, s.Destroy(ctx, conformance.NewPathResource("default", "path-0").Metadata()))

			select {
			case <-time.After(time.Second):
				t.Fatal("timeout waiting for event")
			case ev := <-watchCh:
				assert.Equal(t, state.Destroyed, ev.Type, "ev: %v", ev.Resource)
			}
		}

		for i := range N {
			require.NoError(t, s.Create(ctx, conformance.NewPathResource("default", "path-0")))

			select {
			case <-time.After(time.Second):
				t.Fatalf("timeout waiting for event iteration %d", i)
			case ev := <-watchCh:
				assert.Equal(t, state.Created, ev.Type)
			}

			require.NoError(t, s.Destroy(ctx, conformance.NewPathResource("default", "path-0").Metadata()))

			select {
			case <-time.After(time.Second):
				t.Fatal("timeout waiting for event")
			case ev := <-watchCh:
				assert.Equal(t, state.Destroyed, ev.Type, "ev: %v", ev.Resource)
			}
		}
	})
}

//nolint:gocognit,gocyclo,cyclop
func TestWatchKindStress(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t, goleak.IgnoreCurrent()) })

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		const (
			initialCreated    = 100
			dummyWatches      = 3
			iterations        = 20
			numObjects        = 100
			watchEventTimeout = 10 * time.Second
		)

		for i := range initialCreated {
			require.NoError(t, s.Create(ctx, conformance.NewPathResource("default", fmt.Sprintf("path-%d", i))))
		}

		var watchChannels []chan state.Event

		for iteration := range iterations {
			// test setup:
			// - on each iteration, set yet another watch with bootstrap contents
			// - the new watch should receive 'initialContents'
			// - add new watch to the list of watches
			// - issue create, update, and destroy for numObjects resources
			// - all watch channels should receive each update
			t.Logf("iteration %d", iteration)

			dummyCh := make(chan state.Event)

			// these watches never receive any events, as they use a different namespace
			for j := range dummyWatches {
				require.NoError(t, s.WatchKind(ctx, conformance.NewPathResource(fmt.Sprintf("default-%d", j+iteration*dummyWatches), "").Metadata(), dummyCh, state.WithBootstrapContents(true)))
			}

			watchCh := make(chan state.Event, numObjects)

			require.NoError(t, s.WatchKind(ctx, conformance.NewPathResource("default", "").Metadata(), watchCh, state.WithBootstrapContents(true)))

			for range initialCreated {
				select {
				case <-time.After(watchEventTimeout):
					t.Fatal("timeout waiting for event")
				case ev := <-watchCh:
					assert.Equal(t, state.Created, ev.Type)
				}
			}

			select {
			case <-time.After(watchEventTimeout):
				t.Fatal("timeout waiting for event")
			case ev := <-watchCh:
				assert.Equal(t, state.Bootstrapped, ev.Type)
			}

			watchChannels = append(watchChannels, watchCh)

			for i := range numObjects {
				r := conformance.NewPathResource("default", fmt.Sprintf("o-%d", iteration*numObjects+i))

				// add some metadata to make the object bigger
				for j := range 5 {
					r.Metadata().Labels().Set(fmt.Sprintf("label-%d", j), "prettybigvalueIwanttoputherejusttomakeitbig")
				}

				for j := range 5 {
					r.Metadata().Finalizers().Add(fmt.Sprintf("finalizer-%d", j))
				}

				require.NoError(t, s.Create(ctx, r))

				for j := range 5 {
					r.Metadata().Finalizers().Remove(fmt.Sprintf("finalizer-%d", j))
				}

				require.NoError(t, s.Update(ctx, r))
			}

			for i := range numObjects {
				require.NoError(t, s.Destroy(ctx, conformance.NewPathResource("default", fmt.Sprintf("o-%d", iteration*numObjects+i)).Metadata()))
			}

			for range numObjects {
				for _, watchCh := range watchChannels {
					select {
					case <-time.After(watchEventTimeout):
						t.Fatal("timeout waiting for event")
					case ev := <-watchCh:
						assert.Equal(t, state.Created, ev.Type)
					}

					select {
					case <-time.After(watchEventTimeout):
						t.Fatal("timeout waiting for event")
					case ev := <-watchCh:
						assert.Equal(t, state.Updated, ev.Type)
					}
				}
			}

			for range numObjects {
				for _, watchCh := range watchChannels {
					select {
					case <-time.After(watchEventTimeout):
						t.Fatal("timeout waiting for event")
					case ev := <-watchCh:
						assert.Equal(t, state.Destroyed, ev.Type)
					}
				}
			}
		}
	})
}
