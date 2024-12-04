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
	tmust "github.com/siderolabs/gen/xtesting/must"
	suiterunner "github.com/stretchr/testify/suite"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/cosi-project/state-etcd/pkg/state/impl/etcd"
	"github.com/cosi-project/state-etcd/pkg/util/testhelpers"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func init() {
	must(protobuf.RegisterResource(conformance.IntResourceType, &conformance.IntResource{}))
	must(protobuf.RegisterResource(conformance.StrResourceType, &conformance.StrResource{}))
	must(protobuf.RegisterResource(conformance.SentenceResourceType, &conformance.SentenceResource{}))
}

func TestRuntimeConformance(t *testing.T) {
	t.Parallel()

	testhelpers.WithEtcd(t, func(cli *clientv3.Client) {
		suite := &conformance.RuntimeSuite{
			SetupRuntime: func(rs *conformance.RuntimeSuite) {
				etcdState := etcd.NewState(cli, store.ProtobufMarshaler{}, etcd.WithSalt([]byte("test123")), etcd.WithKeyPrefix(rs.T().Name()))
				rs.State = state.WrapCore(etcdState)
				rs.Runtime = tmust.Value(runtime.NewRuntime(rs.State, logging.DefaultLogger()))(rs.T())
			},
		}

		suiterunner.Run(t, suite)
	})
}
