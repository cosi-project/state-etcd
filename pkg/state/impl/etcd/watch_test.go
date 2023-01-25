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
)

func TestWatchKindWithBootstrap(t *testing.T) {
	t.Parallel()

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
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			withEtcd(t, func(s state.State) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()

				for i := 0; i < 3; i++ {
					require.NoError(t, s.Create(ctx, conformance.NewPathResource("default", fmt.Sprintf("path-%d", i))))
				}

				if test.destroyIsTheLastOperation {
					require.NoError(t, s.Create(ctx, conformance.NewPathResource("default", "path-3")))
					require.NoError(t, s.Destroy(ctx, conformance.NewPathResource("default", "path-3").Metadata()))
				}

				watchCh := make(chan state.Event)

				require.NoError(t, s.WatchKind(ctx, conformance.NewPathResource("default", "").Metadata(), watchCh, state.WithBootstrapContents(true)))

				for i := 0; i < 3; i++ {
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
