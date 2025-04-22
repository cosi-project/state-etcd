// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package etcd_test

import (
	"context"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource/protobuf"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/conformance"
	"github.com/cosi-project/runtime/pkg/state/impl/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/metadata"

	"github.com/cosi-project/state-etcd/pkg/state/impl/etcd"
	"github.com/cosi-project/state-etcd/pkg/util/testhelpers"
)

func init() {
	must(protobuf.RegisterResource(conformance.PathResourceType, &conformance.PathResource{}))
}

func TestPreserveCreated(t *testing.T) {
	res := conformance.NewPathResource("default", "/another-path")

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithCancel(t.Context())
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

func TestDestroy(t *testing.T) {
	t.Parallel()

	res := conformance.NewPathResource("default", "/")

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		var eg errgroup.Group

		err := s.Create(ctx, res)
		assert.NoError(t, err)

		for range 10 {
			eg.Go(func() error {
				err := s.Destroy(ctx, res.Metadata())
				if err != nil && !state.IsNotFoundError(err) {
					return err
				}

				return nil
			})
		}

		require.NoError(t, eg.Wait())
	})
}

func TestClearGRPCMetadata(t *testing.T) {
	t.Parallel()

	res := conformance.NewPathResource("default", "/")

	withEtcd(t, func(s state.State) {
		ctx, cancel := context.WithCancel(t.Context())
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
	withEtcdAndClient(t, func(st state.State, _ *clientv3.Client) {
		f(st)
	})
}

func withEtcdAndClient(t *testing.T, f func(state.State, *clientv3.Client)) {
	testhelpers.WithEtcd(t, func(cli *clientv3.Client) {
		etcdState := etcd.NewState(cli, store.ProtobufMarshaler{}, etcd.WithSalt([]byte("test123")))
		st := state.WrapCore(etcdState)

		f(st, cli)
	})
}
