// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package recstore provides a typed etcd storage for keys.
package recstore

import (
	"context"
	"fmt"

	"github.com/siderolabs/gen/xerrors"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/cosi-project/state-etcd/pkg/util"
)

// RecStore is etcd record storage for the specific type.
type RecStore[T any] struct {
	client    clientv3.KV
	marshal   func(T) ([]byte, error)
	unmarshal func([]byte) (T, error)
	name      string
	prefix    string
}

// New creates a new record store.
func New[T any](
	client clientv3.KV,
	name string,
	prefix string,
	marshal func(T) ([]byte, error),
	unmarshal func([]byte) (T, error),
) *RecStore[T] {
	return &RecStore[T]{
		client:    client,
		name:      name,
		prefix:    prefix,
		marshal:   marshal,
		unmarshal: unmarshal,
	}
}

// Get retrieves and unmarshales the value for the given key.
func (s *RecStore[T]) Get(ctx context.Context) (Result[T], error) {
	ctx = util.ClearContextMeta(ctx)

	etcdKey := s.etcdKey()

	resp, err := s.client.Get(ctx, etcdKey)
	if err != nil {
		return Result[T]{}, fmt.Errorf("error getting key %s: %w", etcdKey, err)
	}

	if len(resp.Kvs) == 0 {
		return Result[T]{}, xerrors.NewTaggedf[NotFoundTag]("key %s not found", etcdKey)
	}

	data, err := s.unmarshal(resp.Kvs[0].Value)
	if err != nil {
		var zero T

		return Result[T]{}, fmt.Errorf("error unmarshaling data for '%T': %w", zero, err)
	}

	return Result[T]{
		Res:     data,
		Version: resp.Kvs[0].Version,
	}, nil
}

// Create marshals and stores the value for the given key for the selected version.
// It returns an error if the key already exists.
func (s *RecStore[T]) Create(ctx context.Context, val T) error {
	etcdKey := s.etcdKey()

	marshal, err := s.marshal(val)
	if err != nil {
		return fmt.Errorf("error marshaling data for '%T': %w", val, err)
	}

	txnResp, err := s.client.Txn(ctx).If(
		clientv3.Compare(clientv3.Version(etcdKey), "=", 0), // not exists check
	).Then(
		clientv3.OpPut(etcdKey, string(marshal)),
		clientv3.OpGet(etcdKey),
	).Commit()
	if err != nil {
		return fmt.Errorf("error creating key %s: %w", etcdKey, err)
	}

	if !txnResp.Succeeded {
		return xerrors.NewTaggedf[AlreadyExistsTag]("key %s already exists", etcdKey)
	}

	return nil
}

// Update marshals and stores the value for the given key for the selected version.
// If version is 0, it will create a new record.
// If version is not 0, it will update the record for the selected version.
func (s *RecStore[T]) Update(ctx context.Context, val T, version int64) error {
	if version == 0 {
		return s.Create(ctx, val)
	}

	ctx = util.ClearContextMeta(ctx)

	etcdKey := s.etcdKey()

	getResp, err := s.client.Get(ctx, etcdKey)
	if err != nil {
		return fmt.Errorf("error getting key %s: %w", etcdKey, err)
	}

	if len(getResp.Kvs) == 0 {
		return xerrors.NewTaggedf[NotFoundTag]("key %s not found", etcdKey)
	}

	actualVersion := getResp.Kvs[0].Version
	if version != actualVersion {
		return xerrors.NewTaggedf[VersionConflictTag]("key %s version mismatch, expected '%d' actual '%d'", etcdKey, version, actualVersion)
	}

	marshal, err := s.marshal(val)
	if err != nil {
		return fmt.Errorf("error marshaling data for '%T': %w", val, err)
	}

	txnResp, err := s.client.Txn(ctx).If(
		clientv3.Compare(clientv3.Version(etcdKey), "=", version),
	).Then(
		clientv3.OpPut(etcdKey, string(marshal)),
		clientv3.OpGet(etcdKey),
	).Else(
		clientv3.OpGet(etcdKey),
	).Commit()
	if err != nil {
		return fmt.Errorf("error updating key %s: %w", etcdKey, err)
	}

	if !txnResp.Succeeded {
		var foundVersion int64

		txnGetKvs := txnResp.Responses[0].GetResponseRange().Kvs
		if len(txnResp.Responses[0].GetResponseRange().Kvs) > 0 {
			foundVersion = txnGetKvs[0].Version
		}

		return xerrors.NewTaggedf[TxVersionConflictTag]("key %s tx version mismatch, expected '%d' actual '%d'", etcdKey, version, foundVersion)
	}

	return nil
}

// Result is a result of [RecStore.Get].
type Result[T any] struct {
	Res     T
	Version int64
}

func (s *RecStore[T]) etcdKey() string {
	return s.prefix + "/record-store/" + s.name
}

type (
	// NotFoundTag is an error tag for not found errors.
	NotFoundTag struct{}
	// VersionConflictTag is an error tag when the version of the record does not match the expected version.
	VersionConflictTag struct{}
	// TxVersionConflictTag is an error tag when version conflict that happens in a transaction.
	TxVersionConflictTag struct{}
	// AlreadyExistsTag is an error tag that is returned when a key already exists.
	AlreadyExistsTag struct{}
)
