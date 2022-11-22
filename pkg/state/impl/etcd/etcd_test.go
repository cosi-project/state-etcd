// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package etcd_test

import (
	"context"
	"log"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/resource/protobuf"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/conformance"
	"github.com/cosi-project/runtime/pkg/state/impl/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/metadata"

	"github.com/cosi-project/state-etcd/pkg/state/impl/etcd"
	"github.com/cosi-project/state-etcd/pkg/util/testhelpers"
)

func init() {
	err := protobuf.RegisterResource(conformance.PathResourceType, &conformance.PathResource{})
	if err != nil {
		log.Fatalf("failed to register resource: %v", err)
	}
}

func TestEtcdConformance(t *testing.T) {
	t.Parallel()

	withEtcd(t, func(s state.State) {
		suite.Run(t, &conformance.StateSuite{
			State:      s,
			Namespaces: []resource.Namespace{"default", "controller", "system", "runtime"},
		})
	})
}

func TestPreserveCreated(t *testing.T) {
	res := conformance.NewPathResource("default", "/another-path")

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ctx = metadata.NewIncomingContext(
			ctx,
			metadata.Pairs("authorization", "bearer something"),
		)

		err := s.Create(ctx, res)
		assert.NoError(t, err)

		got, err := s.Get(ctx, res.Metadata())
		assert.NoError(t, err)
		assert.NotZero(t, got.Metadata().Created())

		cpy := got.DeepCopy()
		cpy.Metadata().SetCreated(time.Time{})

		err = s.Update(ctx, cpy)
		assert.NoError(t, err)

		result, err := s.Get(ctx, cpy.Metadata())
		assert.NoError(t, err)

		assert.Equal(t, got.Metadata().Created(), result.Metadata().Created())
	})
}

func TestClearGRPCMetadata(t *testing.T) {
	t.Parallel()

	res := conformance.NewPathResource("default", "/")

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// the authorization header causes embedded etcd to return an error if it is passed through
		ctx = metadata.NewIncomingContext(
			ctx,
			metadata.Pairs("authorization", "bearer something"),
		)

		err := s.Create(ctx, res)
		assert.NoError(t, err)

		_, err = s.Get(ctx, res.Metadata())
		assert.NoError(t, err)

		_, err = s.List(ctx, res.Metadata())
		assert.NoError(t, err)

		err = s.Update(ctx, res)
		assert.NoError(t, err)

		err = s.Watch(ctx, res.Metadata(), nil)
		assert.NoError(t, err)

		err = s.WatchKind(ctx, res.Metadata(), nil)
		assert.NoError(t, err)

		err = s.Destroy(ctx, res.Metadata())
		assert.NoError(t, err)
	})
}

func withEtcd(t *testing.T, f func(state.State)) {
	testhelpers.WithEtcd(t, func(cli *clientv3.Client) {
		etcdState := etcd.NewState(cli, store.ProtobufMarshaler{}, etcd.WithSalt([]byte("test123")))
		st := state.WrapCore(etcdState)

		f(st)
	})
}
