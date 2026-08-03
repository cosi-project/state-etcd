// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package etcd

import (
	"context"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
)

// ObserverFunc is invoked after a successful Create, Update, or Destroy operation.
// Teardown is implemented as an Update that transitions the phase from resource.PhaseRunning
// to resource.PhaseTearingDown.
type ObserverFunc func(ctx context.Context, eventType state.EventType, resourceType resource.Type, phase, previousPhase resource.Phase, marshaledBytes int) error

// LimiterFunc is invoked before each Create, Update, or Destroy etcd transaction.
// Returning a non-nil error aborts the mutation and propagates the error to the caller.
// For Create/Update, marshaledBytes is the size of the payload about to be written.
// For Destroy, marshaledBytes is the size of the existing value about to be deleted.
type LimiterFunc func(ctx context.Context, eventType state.EventType, resourceType resource.Type, phase, previousPhase resource.Phase, marshaledBytes int) error

// StateOptions configure etcd.State.
type StateOptions struct {
	observer    ObserverFunc
	limiter     LimiterFunc
	keyPrefix   string
	salt        []byte
	sharedWatch bool
}

// StateOption applies settings to StateOptions.
type StateOption func(options *StateOptions)

// DefaultStateOptions returns default value of StateOptions.
func DefaultStateOptions() StateOptions {
	return StateOptions{
		keyPrefix: "/cosi",
	}
}

// WithSalt sets the salt to be used in the etcd keys for StateOptions.
func WithSalt(salt []byte) StateOption {
	return func(options *StateOptions) {
		options.salt = make([]byte, len(salt))
		copy(options.salt, salt)
	}
}

// WithKeyPrefix sets the global prefix to be used in the etcd keys for StateOptions.
// Defaults to "/cosi" if not specified.
func WithKeyPrefix(keyPrefix string) StateOption {
	return func(options *StateOptions) {
		options.keyPrefix = keyPrefix
	}
}

// WithObserver registers a callback that is invoked after each successful Create, Update,
// or Destroy operation. It can be used by callers to record metrics such as operation
// counts and marshaled byte sizes.
func WithObserver(fn ObserverFunc) StateOption {
	return func(options *StateOptions) {
		options.observer = fn
	}
}

// WithLimiter registers a callback that is invoked before each Create, Update, or Destroy
// etcd transaction. The callback may return an error to abort the mutation; the error is
// propagated to the caller without any state change.
//
// The limiter runs after local validations (owner, version, phase, finalizers) and before
// the etcd transaction. For Create, the existence check is part of the transaction itself,
// so the limiter may fire for a key that turns out to already exist. For Update and Destroy,
// the transaction may still fail due to a concurrent writer after the limiter accepts.
// Limiters that count against a budget should tolerate these post-accept failures.
func WithLimiter(fn LimiterFunc) StateOption {
	return func(options *StateOptions) {
		options.limiter = fn
	}
}

// WithSharedWatch makes all watches created by the State share a single etcd watcher established
// over the whole key prefix, with a single goroutine dispatching the events to the subscribers.
//
// Without this option every Watch/WatchKind/WatchKindAggregated call establishes its own etcd
// watcher, and the etcd client library delivers each watcher's responses on its own goroutine, so
// there is no ordering relationship between the streams: an event for revision N might be
// delivered after an event for revision N+1 which belongs to a different resource type. Consumers
// merging several watches into one stream - the COSI controller runtime being one - observe that
// as resources appearing out of causal order.
//
// With this option, events delivered to the same destination channel are in etcd revision order,
// no matter which watch produced them. The guarantee is per channel rather than global: consumers
// routinely read several watch channels in a fixed order, and a dispatcher ordering across all of
// them would have to block on one channel while the consumer waits on another. For the same
// reason a slow consumer only holds up the watches writing to the channel it is reading, not every
// watch of the State.
//
// The cost is that the shared watcher receives the events of every resource type under the key
// prefix, including those nobody is watching, in exchange for establishing one etcd watcher
// instead of one per watch call.
//
// Watches started with state.WithStartFromBookmark are not served by the shared watcher: it is
// already positioned past that revision and cannot replay history. Such watches fall back to a
// dedicated etcd watcher and are not ordered against the rest.
func WithSharedWatch() StateOption {
	return func(options *StateOptions) {
		options.sharedWatch = true
	}
}
