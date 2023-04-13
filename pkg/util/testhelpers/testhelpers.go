// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package testhelpers provides helpers for tests.
package testhelpers

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/embed"
	"go.etcd.io/etcd/server/v3/etcdserver/api/v3client"
)

// WithEtcd runs the test with an embedded etcd server.
func WithEtcd(t *testing.T, f func(*clientv3.Client)) {
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

	cfg.ListenPeerUrls = []url.URL{*peerURL}
	cfg.ListenClientUrls = []url.URL{*clientURL}

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

	defer func() {
		err := cli.Close()
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("failed to close etcd client: %v", err)
		}
	}()

	f(cli)
}
