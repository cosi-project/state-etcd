// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package etcd_test

import (
	"testing"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/stretchr/testify/require"

	"github.com/cosi-project/state-etcd/pkg/state/impl/etcd"
)

func TestErrors(t *testing.T) {
	res := resource.NewMetadata("ns", "a", "b", resource.VersionUndefined)

	require.Implements(t, (*state.ErrConflict)(nil), etcd.ErrAlreadyExists(res))
	require.Implements(t, (*state.ErrConflict)(nil), etcd.ErrVersionConflict(res, 1, 2))
	require.Implements(t, (*state.ErrConflict)(nil), etcd.ErrOwnerConflict(res, "owner"))
	require.Implements(t, (*state.ErrConflict)(nil), etcd.ErrPendingFinalizers(res))
	require.Implements(t, (*state.ErrConflict)(nil), etcd.ErrPhaseConflict(res, resource.PhaseRunning))
	require.Implements(t, (*state.ErrNotFound)(nil), etcd.ErrNotFound(res))

	require.True(t, state.IsConflictError(etcd.ErrAlreadyExists(res), state.WithResourceType("a")))
	require.False(t, state.IsConflictError(etcd.ErrAlreadyExists(res), state.WithResourceType("b")))
	require.True(t, state.IsConflictError(etcd.ErrAlreadyExists(res), state.WithResourceNamespace("ns")))
}
