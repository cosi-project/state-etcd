// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package etcd_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/conformance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cosi-project/state-etcd/pkg/state/impl/etcd"
)

type observation struct {
	rType         resource.Type
	phase         resource.Phase
	previousPhase resource.Phase
	eventType     state.EventType
	bytes         int
}

type recordingObserver struct {
	observed []observation
	mu       sync.Mutex
}

func (r *recordingObserver) record(_ context.Context, eventType state.EventType, rType resource.Type, phase, previousPhase resource.Phase, marshaledBytes int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.observed = append(r.observed, observation{
		rType:         rType,
		phase:         phase,
		previousPhase: previousPhase,
		eventType:     eventType,
		bytes:         marshaledBytes,
	})

	return nil
}

func (r *recordingObserver) snapshot() []observation {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.observed)
}

func TestObserverFiresOnSuccess(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		res := conformance.NewPathResource("default", "/observer-success")

		require.NoError(t, s.Create(ctx, res))
		require.NoError(t, s.Update(ctx, res))
		require.NoError(t, s.Destroy(ctx, res.Metadata()))

		got := obs.snapshot()
		require.Len(t, got, 3)

		assert.Equal(t, state.Created, got[0].eventType)
		assert.Equal(t, res.Metadata().Type(), got[0].rType)
		assert.Equal(t, resource.PhaseRunning, got[0].phase)
		assert.Equal(t, got[0].phase, got[0].previousPhase)
		assert.Positive(t, got[0].bytes)

		assert.Equal(t, state.Updated, got[1].eventType)
		assert.Equal(t, res.Metadata().Type(), got[1].rType)
		assert.Equal(t, resource.PhaseRunning, got[1].phase)
		assert.Equal(t, resource.PhaseRunning, got[1].previousPhase)
		assert.Positive(t, got[1].bytes)

		assert.Equal(t, state.Destroyed, got[2].eventType)
		assert.Equal(t, res.Metadata().Type(), got[2].rType)
		assert.Equal(t, resource.PhaseRunning, got[2].phase)
		assert.Equal(t, got[2].phase, got[2].previousPhase)
		// destroy reports the size of the previously stored resource
		assert.Equal(t, got[1].bytes, got[2].bytes)
	}, etcd.WithObserver(obs.record))
}

func TestObserverPhaseOnTeardown(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		res := conformance.NewPathResource("default", "/observer-teardown")

		require.NoError(t, s.Create(ctx, res))

		// Teardown is implemented as an Update that flips the phase to tearing-down.
		// previousPhase must reflect the running phase so callers can distinguish the actual
		// transition from updates that happen while already in tearing-down (see below).
		_, err := s.Teardown(ctx, res.Metadata())
		require.NoError(t, err)

		// Add and remove a finalizer while the resource is in tearing-down. Each operation is
		// an Update where both phase and previousPhase are PhaseTearingDown — observers should
		// not mistake these for a teardown transition.
		require.NoError(t, s.AddFinalizer(ctx, res.Metadata(), "fin"))
		require.NoError(t, s.RemoveFinalizer(ctx, res.Metadata(), "fin"))

		require.NoError(t, s.Destroy(ctx, res.Metadata()))

		got := obs.snapshot()
		require.Len(t, got, 5)

		assert.Equal(t, state.Created, got[0].eventType)
		assert.Equal(t, resource.PhaseRunning, got[0].phase)
		assert.Equal(t, got[0].phase, got[0].previousPhase)

		// Teardown — phase transitions running → tearingDown.
		assert.Equal(t, state.Updated, got[1].eventType)
		assert.Equal(t, resource.PhaseTearingDown, got[1].phase)
		assert.Equal(t, resource.PhaseRunning, got[1].previousPhase)

		// AddFinalizer — Update while in tearing-down. No transition.
		assert.Equal(t, state.Updated, got[2].eventType)
		assert.Equal(t, resource.PhaseTearingDown, got[2].phase)
		assert.Equal(t, resource.PhaseTearingDown, got[2].previousPhase)

		// RemoveFinalizer — Update while in tearing-down. No transition.
		assert.Equal(t, state.Updated, got[3].eventType)
		assert.Equal(t, resource.PhaseTearingDown, got[3].phase)
		assert.Equal(t, resource.PhaseTearingDown, got[3].previousPhase)

		assert.Equal(t, state.Destroyed, got[4].eventType)
		assert.Equal(t, resource.PhaseTearingDown, got[4].phase)
		assert.Equal(t, got[4].phase, got[4].previousPhase)
	}, etcd.WithObserver(obs.record))
}

func TestObserverDoesNotFireOnFailure(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		res := conformance.NewPathResource("default", "/observer-failure")

		// create twice - second should fail with already exists, no observation
		require.NoError(t, s.Create(ctx, res))

		err := s.Create(ctx, res)
		require.Error(t, err)
		assert.True(t, state.IsConflictError(err))

		// update a non-existent resource - no observation
		missing := conformance.NewPathResource("default", "/observer-missing")
		err = s.Update(ctx, missing)
		require.Error(t, err)
		assert.True(t, state.IsNotFoundError(err))

		// destroy a non-existent resource - no observation
		err = s.Destroy(ctx, missing.Metadata())
		require.Error(t, err)
		assert.True(t, state.IsNotFoundError(err))

		// only the first successful create should be observed
		got := obs.snapshot()
		require.Len(t, got, 1)
		assert.Equal(t, state.Created, got[0].eventType)
	}, etcd.WithObserver(obs.record))
}

func TestObserverErrorPropagates(t *testing.T) {
	t.Parallel()

	observerErr := errors.New("observer broke")

	failing := func(context.Context, state.EventType, resource.Type, resource.Phase, resource.Phase, int) error {
		return observerErr
	}

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		// Create: observer fires post-success, error propagates back. The resource is in etcd.
		res := conformance.NewPathResource("default", "/observer-error")

		err := s.Create(ctx, res)
		require.Error(t, err)
		assert.ErrorIs(t, err, observerErr)

		// The underlying mutation succeeded even though the observer error propagated.
		got, getErr := s.Get(ctx, res.Metadata())
		require.NoError(t, getErr)
		assert.Equal(t, res.Metadata().ID(), got.Metadata().ID())

		// Update: observer error propagates again; mutation still committed.
		err = s.Update(ctx, got)
		require.Error(t, err)
		assert.ErrorIs(t, err, observerErr)

		// Destroy: observer error propagates; resource is gone from etcd.
		err = s.Destroy(ctx, res.Metadata())
		require.Error(t, err)
		assert.ErrorIs(t, err, observerErr)

		_, getErr = s.Get(ctx, res.Metadata())
		assert.True(t, state.IsNotFoundError(getErr))
	}, etcd.WithObserver(failing))
}

func TestObserverPanicPropagates(t *testing.T) {
	t.Parallel()

	panicking := func(context.Context, state.EventType, resource.Type, resource.Phase, resource.Phase, int) error {
		panic("observer boom")
	}

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		res := conformance.NewPathResource("default", "/observer-panic")

		// Create: observer panic is recovered and surfaced as an error; mutation is committed.
		err := s.Create(ctx, res)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "observer panicked")
		assert.Contains(t, err.Error(), "observer boom")

		got, getErr := s.Get(ctx, res.Metadata())
		require.NoError(t, getErr)
		assert.Equal(t, res.Metadata().ID(), got.Metadata().ID())

		// Update: same behavior.
		err = s.Update(ctx, got)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "observer panicked")

		// Destroy: same behavior; resource is gone from etcd.
		err = s.Destroy(ctx, res.Metadata())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "observer panicked")

		_, getErr = s.Get(ctx, res.Metadata())
		assert.True(t, state.IsNotFoundError(getErr))
	}, etcd.WithObserver(panicking))
}

func TestObserverNilNoop(t *testing.T) {
	t.Parallel()

	// no observer configured: operations succeed without panic
	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		res := conformance.NewPathResource("default", "/observer-nil")

		require.NoError(t, s.Create(ctx, res))
		require.NoError(t, s.Update(ctx, res))
		require.NoError(t, s.Destroy(ctx, res.Metadata()))
	})
}
