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

type limitCall struct {
	rType         resource.Type
	phase         resource.Phase
	previousPhase resource.Phase
	eventType     state.EventType
	bytes         int
}

type recordingLimiter struct {
	rejects map[state.EventType]error
	calls   []limitCall
	mu      sync.Mutex
}

func (r *recordingLimiter) gate(_ context.Context, eventType state.EventType, rType resource.Type, phase, previousPhase resource.Phase, marshaledBytes int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.calls = append(r.calls, limitCall{
		rType:         rType,
		phase:         phase,
		previousPhase: previousPhase,
		eventType:     eventType,
		bytes:         marshaledBytes,
	})

	if err, ok := r.rejects[eventType]; ok {
		return err
	}

	return nil
}

func (r *recordingLimiter) snapshot() []limitCall {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.calls)
}

func TestLimiterFiresBeforeMutation(t *testing.T) {
	t.Parallel()

	lim := &recordingLimiter{}

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		res := conformance.NewPathResource("default", "/limiter-success")

		require.NoError(t, s.Create(ctx, res))
		require.NoError(t, s.Update(ctx, res))
		require.NoError(t, s.Destroy(ctx, res.Metadata()))

		got := lim.snapshot()
		require.Len(t, got, 3)

		assert.Equal(t, state.Created, got[0].eventType)
		assert.Positive(t, got[0].bytes)

		assert.Equal(t, state.Updated, got[1].eventType)
		assert.Positive(t, got[1].bytes)

		assert.Equal(t, state.Destroyed, got[2].eventType)
		// destroy reports the size of the previously stored payload
		assert.Equal(t, got[1].bytes, got[2].bytes)
	}, etcd.WithLimiter(lim.gate))
}

func TestLimiterRejectionAbortsMutation(t *testing.T) {
	t.Parallel()

	createErr := errors.New("limiter rejected create")

	lim := &recordingLimiter{rejects: map[state.EventType]error{state.Created: createErr}}

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		res := conformance.NewPathResource("default", "/limiter-reject-create")

		err := s.Create(ctx, res)
		require.Error(t, err)
		assert.ErrorIs(t, err, createErr)

		// rejected mutation did not commit
		_, getErr := s.Get(ctx, res.Metadata())
		assert.True(t, state.IsNotFoundError(getErr))
	}, etcd.WithLimiter(lim.gate))
}

func TestLimiterDoesNotFireOnPreconditionFailures(t *testing.T) {
	t.Parallel()

	lim := &recordingLimiter{}

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		res := conformance.NewPathResource("default", "/limiter-precondition")

		// Bring the resource into existence; one Create gate call.
		require.NoError(t, s.Create(ctx, res))

		// Updating a missing resource fails in the precondition Get, before the limiter.
		missing := conformance.NewPathResource("default", "/limiter-missing")
		err := s.Update(ctx, missing)
		require.Error(t, err)
		assert.True(t, state.IsNotFoundError(err))

		// Destroying a missing resource fails the same way.
		err = s.Destroy(ctx, missing.Metadata())
		require.Error(t, err)
		assert.True(t, state.IsNotFoundError(err))

		got := lim.snapshot()
		require.Len(t, got, 1)
		assert.Equal(t, state.Created, got[0].eventType)
	}, etcd.WithLimiter(lim.gate))
}

func TestLimiterPanicPropagates(t *testing.T) {
	t.Parallel()

	panicking := func(context.Context, state.EventType, resource.Type, resource.Phase, resource.Phase, int) error {
		panic("limiter boom")
	}

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		res := conformance.NewPathResource("default", "/limiter-panic")

		err := s.Create(ctx, res)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "limiter panicked")
		assert.Contains(t, err.Error(), "limiter boom")

		// mutation was aborted; resource not in etcd
		_, getErr := s.Get(ctx, res.Metadata())
		assert.True(t, state.IsNotFoundError(getErr))
	}, etcd.WithLimiter(panicking))
}

func TestLimiterNilNoop(t *testing.T) {
	t.Parallel()

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		res := conformance.NewPathResource("default", "/limiter-nil")

		require.NoError(t, s.Create(ctx, res))
		require.NoError(t, s.Update(ctx, res))
		require.NoError(t, s.Destroy(ctx, res.Metadata()))
	})
}
