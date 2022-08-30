// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package etcd

// StateOptions configure inmem.State.
type StateOptions struct {
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
