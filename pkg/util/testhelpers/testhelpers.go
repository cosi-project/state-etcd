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
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// WithEtcd runs the test with an embedded etcd server.
func WithEtcd(t *testing.T, f func(*clientv3.Client)) {
	tempDir := t.TempDir()

	cfg := embed.NewConfig()
	cfg.Dir = tempDir

	cfg.EnableGRPCGateway = false
	cfg.LogLevel = "info"
	cfg.ZapLoggerBuilder = embed.NewZapLoggerBuilder(
		zaptest.NewLogger(
			t,
			zaptest.Level(zap.InfoLevel),
			zaptest.WrapOptions(
				zap.Fields(zap.String("component", "etcd-server")),
			),
		),
	)
	cfg.AuthToken = ""
	cfg.AutoCompactionMode = "periodic"
	cfg.AutoCompactionRetention = "5h"
	cfg.ExperimentalCompactHashCheckEnabled = true
	cfg.ExperimentalInitialCorruptCheck = true
	cfg.UnsafeNoFsync = true

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

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	select {
	case <-e.Server.ReadyNotify():
	case <-ctx.Done():
		t.Fatalf("etcd failed to start")
	}

	defer func() {
		e.Close()

		shutdownCtx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()

		select {
		case <-e.Server.StopNotify():
		case <-shutdownCtx.Done():
			t.Fatalf("etcd failed to stop")
		}
	}()

	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{e.Clients[0].Addr().String()},
		DialTimeout: time.Second,
		Logger: zaptest.NewLogger(
			t,
			zaptest.Level(zap.InfoLevel),
			zaptest.WrapOptions(
				zap.Fields(zap.String("component", "etcd-client")),
			),
		),
	})
	if err != nil {
		t.Fatalf("failed to create etcd client: %v", err)
	}

	defer func() {
		err := cli.Close()
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("failed to close etcd client: %v", err)
		}
	}()

	f(cli)
}
