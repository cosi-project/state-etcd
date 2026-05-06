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

// StateOptions configure etcd.State.
type StateOptions struct {
	observer  ObserverFunc
	keyPrefix string
	salt      []byte
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
