// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package etcd_test

import (
	"testing"

	"github.com/cosi-project/runtime/pkg/controller/conformance"
	"github.com/cosi-project/runtime/pkg/controller/runtime"
	"github.com/cosi-project/runtime/pkg/logging"
	"github.com/cosi-project/runtime/pkg/resource/protobuf"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/impl/store"
	"github.com/stretchr/testify/require"
	suiterunner "github.com/stretchr/testify/suite"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/cosi-project/state-etcd/pkg/state/impl/etcd"
	"github.com/cosi-project/state-etcd/pkg/util/testhelpers"
)

func TestRuntimeConformance(t *testing.T) {
	t.Parallel()

	require.NoError(t, protobuf.RegisterResource(conformance.IntResourceType, &conformance.IntResource{}))
	require.NoError(t, protobuf.RegisterResource(conformance.StrResourceType, &conformance.StrResource{}))
	require.NoError(t, protobuf.RegisterResource(conformance.SentenceResourceType, &conformance.SentenceResource{}))

	testhelpers.WithEtcd(t, func(cli *clientv3.Client) {
		suite := &conformance.RuntimeSuite{}
		suite.SetupRuntime = func() {
			etcdState := etcd.NewState(cli, store.ProtobufMarshaler{}, etcd.WithSalt([]byte("test123")), etcd.WithKeyPrefix(suite.T().Name()))

			suite.State = state.WrapCore(etcdState)

			var err error

			logger := logging.DefaultLogger()

			suite.Runtime, err = runtime.NewRuntime(suite.State, logger)
			suite.Require().NoError(err)
		}

		suiterunner.Run(t, suite)
	})
}
