// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package etcd_test

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/resource/protobuf"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/conformance"
	"github.com/cosi-project/runtime/pkg/state/impl/store"
	"github.com/stretchr/testify/suite"
	"go.etcd.io/etcd/server/v3/embed"
	"go.etcd.io/etcd/server/v3/etcdserver/api/v3client"

	"github.com/cosi-project/state-etcd/pkg/state/impl/etcd"
)

func TestEtcdConformance(t *testing.T) {
	t.Parallel()

	err := protobuf.RegisterResource(conformance.PathResourceType, &conformance.PathResource{})
	if err != nil {
		t.Fatalf("failed to register resource: %v", err)
	}

	withEtcd(t, func(s state.State) {
		suite.Run(t, &conformance.StateSuite{
			State:      s,
			Namespaces: []resource.Namespace{"default", "controller", "system", "runtime"},
		})
	})
}

func withEtcd(t *testing.T, f func(state.State)) {
	tempDir := t.TempDir()

	cfg := embed.NewConfig()
	cfg.Dir = tempDir

	peerURL, err := url.Parse("http://localhost:0")
	if err != nil {
		t.Fatalf("failed to parse URL: %v", err)
	}

	clientURL, err := url.Parse("http://localhost:0")
	if err != nil {
		t.Fatalf("failed to parse URL: %v", err)
	}

	cfg.LPUrls = []url.URL{*peerURL}
	cfg.LCUrls = []url.URL{*clientURL}

	e, err := embed.StartEtcd(cfg)
	if err != nil {
		t.Fatalf("failed to start etcd: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	select {
	case <-e.Server.ReadyNotify():
	case <-ctx.Done():
		t.Fatalf("etcd failed to start")
	}

	defer func() {
		e.Close()

		select {
		case <-e.Server.StopNotify():
		case <-ctx.Done():
			t.Fatalf("etcd failed to stop")
		}
	}()

	cli := v3client.New(e.Server)

	defer cli.Close() //nolint:errcheck

	etcdState := etcd.NewState(cli, store.ProtobufMarshaler{}, etcd.WithSalt([]byte("test123")))
	st := state.WrapCore(etcdState)

	f(st)
}
