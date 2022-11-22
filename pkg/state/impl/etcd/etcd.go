// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package etcd provides an implementation of state.State in etcd.
package etcd

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/impl/store"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/cosi-project/state-etcd/pkg/util"
)

// Client is the etcd client interface required by this state implementation.
type Client interface {
	clientv3.KV
	clientv3.Watcher
}

// State implements state.CoreState.
type State struct {
	cli       Client
	marshaler store.Marshaler
	keyPrefix string
	salt      []byte
}

// Check interface implementation.
var _ state.CoreState = &State{}

// NewState creates new State with default options.
func NewState(cli Client, marshaler store.Marshaler, opts ...StateOption) *State {
	options := DefaultStateOptions()

	for _, opt := range opts {
		opt(&options)
	}

	return &State{
		cli:       cli,
		marshaler: marshaler,
		keyPrefix: options.keyPrefix,
		salt:      options.salt,
	}
}

// Get a resource.
func (st *State) Get(ctx context.Context, resourcePointer resource.Pointer, opts ...state.GetOption) (resource.Resource, error) { //nolint:ireturn
	ctx = st.clearIncomingContext(ctx)

	var options state.GetOptions

	for _, opt := range opts {
		opt(&options)
	}

	etcdKey := st.etcdKeyFromPointer(resourcePointer)

	resp, err := st.cli.Get(ctx, etcdKey)
	if err != nil {
		return nil, err
	}

	if len(resp.Kvs) == 0 {
		return nil, ErrNotFound(resourcePointer)
	}

	return st.unmarshalResource(resp.Kvs[0])
}

// List resources.
func (st *State) List(ctx context.Context, resourceKind resource.Kind, opts ...state.ListOption) (resource.List, error) {
	ctx = st.clearIncomingContext(ctx)

	var options state.ListOptions

	for _, opt := range opts {
		opt(&options)
	}

	etcdKey := st.etcdKeyPrefixFromKind(resourceKind)

	resp, err := st.cli.Get(ctx, etcdKey, clientv3.WithPrefix())
	if err != nil {
		return resource.List{}, err
	}

	resources := make([]resource.Resource, 0, len(resp.Kvs))

	for _, kv := range resp.Kvs {
		res, err := st.unmarshalResource(kv)
		if err != nil {
			return resource.List{}, err
		}

		if !options.LabelQuery.Matches(*res.Metadata().Labels()) {
			continue
		}

		resources = append(resources, res)
	}

	sort.Slice(resources, func(i, j int) bool {
		return strings.Compare(resources[i].Metadata().String(), resources[j].Metadata().String()) < 0
	})

	return resource.List{
		Items: resources,
	}, nil
}

// Create a resource.
func (st *State) Create(ctx context.Context, res resource.Resource, opts ...state.CreateOption) error {
	ctx = st.clearIncomingContext(ctx)

	resCopy := res.DeepCopy()

	var options state.CreateOptions

	for _, opt := range opts {
		opt(&options)
	}

	etcdKey := st.etcdKeyFromPointer(resCopy.Metadata())

	if err := resCopy.Metadata().SetOwner(options.Owner); err != nil {
		return err
	}

	resCopy.Metadata().SetCreated(time.Now())

	data, err := st.marshaler.MarshalResource(resCopy)
	if err != nil {
		return err
	}

	txnResp, err := st.cli.Txn(ctx).If(
		clientv3.Compare(clientv3.Version(etcdKey), "=", 0), // not exists check
	).Then(
		clientv3.OpPut(etcdKey, string(data)),
		clientv3.OpGet(etcdKey),
	).Commit()
	if err != nil {
		return err
	}

	if !txnResp.Succeeded {
		return ErrAlreadyExists(resCopy.Metadata())
	}

	versionStr := strconv.FormatInt(txnResp.Responses[1].GetResponseRange().Kvs[0].Version, 10)

	version, err := resource.ParseVersion(versionStr)
	if err != nil {
		return err
	}

	resCopy.Metadata().SetVersion(version)
	// This should be safe, because we don't allow to share metadata between goroutines even for read-only
	// purposes.
	*res.Metadata() = *resCopy.Metadata()

	return nil
}

// Update a resource.
func (st *State) Update(ctx context.Context, res resource.Resource, opts ...state.UpdateOption) error {
	ctx = st.clearIncomingContext(ctx)

	resCopy := res.DeepCopy()

	options := state.DefaultUpdateOptions()

	for _, opt := range opts {
		opt(&options)
	}

	etcdKey := st.etcdKeyFromPointer(resCopy.Metadata())

	getResp, err := st.cli.Get(ctx, etcdKey)
	if err != nil {
		return err
	}

	if len(getResp.Kvs) == 0 {
		return ErrNotFound(resCopy.Metadata())
	}

	curResource, err := st.unmarshalResource(getResp.Kvs[0])
	if err != nil {
		return err
	}

	updated := time.Now()

	resCopy.Metadata().SetUpdated(updated)
	resCopy.Metadata().SetCreated(curResource.Metadata().Created())

	data, err := st.marshaler.MarshalResource(resCopy)
	if err != nil {
		return err
	}

	expectedVersion := int64(resCopy.Metadata().Version().Value())

	etcdVersion := getResp.Kvs[0].Version

	if etcdVersion != expectedVersion {
		return ErrVersionConflict(curResource.Metadata(), expectedVersion, etcdVersion)
	}

	if curResource.Metadata().Owner() != options.Owner {
		return ErrOwnerConflict(curResource.Metadata(), curResource.Metadata().Owner())
	}

	if options.ExpectedPhase != nil && curResource.Metadata().Phase() != *options.ExpectedPhase {
		return ErrPhaseConflict(curResource.Metadata(), *options.ExpectedPhase)
	}

	txnResp, err := st.cli.Txn(ctx).If(
		clientv3.Compare(clientv3.Version(etcdKey), "=", etcdVersion),
	).Then(
		clientv3.OpPut(etcdKey, string(data)),
		clientv3.OpGet(etcdKey),
	).Else(
		clientv3.OpGet(etcdKey),
	).Commit()
	if err != nil {
		return err
	}

	if !txnResp.Succeeded {
		var foundVersion int64

		txnGetKvs := txnResp.Responses[0].GetResponseRange().Kvs
		if len(txnResp.Responses[0].GetResponseRange().Kvs) > 0 {
			foundVersion = txnGetKvs[0].Version
		}

		return ErrVersionConflict(resCopy.Metadata(), expectedVersion, foundVersion)
	}

	versionStr := strconv.FormatInt(txnResp.Responses[1].GetResponseRange().Kvs[0].Version, 10)

	version, err := resource.ParseVersion(versionStr)
	if err != nil {
		return err
	}

	resCopy.Metadata().SetVersion(version)
	// This should be safe, because we don't allow to share metadata between goroutines even for read-only
	// purposes.
	*res.Metadata() = *resCopy.Metadata()

	return nil
}

// Destroy a resource.
func (st *State) Destroy(ctx context.Context, resourcePointer resource.Pointer, opts ...state.DestroyOption) error {
	ctx = st.clearIncomingContext(ctx)

	var options state.DestroyOptions

	for _, opt := range opts {
		opt(&options)
	}

	etcdKey := st.etcdKeyFromPointer(resourcePointer)

	resp, err := st.cli.Get(ctx, etcdKey)
	if err != nil {
		return err
	}

	if len(resp.Kvs) == 0 {
		return ErrNotFound(resourcePointer)
	}

	etcdVersion := resp.Kvs[0].Version

	curResource, err := st.unmarshalResource(resp.Kvs[0])
	if err != nil {
		return err
	}

	if curResource.Metadata().Owner() != options.Owner {
		return ErrOwnerConflict(curResource.Metadata(), curResource.Metadata().Owner())
	}

	if !curResource.Metadata().Finalizers().Empty() {
		return ErrPendingFinalizers(*curResource.Metadata())
	}

	txnResp, err := st.cli.Txn(ctx).If(
		clientv3.Compare(clientv3.Version(etcdKey), "=", etcdVersion),
	).Then(
		clientv3.OpDelete(etcdKey),
	).Else(
		clientv3.OpGet(etcdKey),
	).Commit()
	if err != nil {
		return err
	}

	if txnResp.Succeeded {
		return nil
	}

	var foundVersion int64

	txnGetKvs := txnResp.Responses[0].GetResponseRange().Kvs
	if len(txnResp.Responses[0].GetResponseRange().Kvs) > 0 {
		foundVersion = txnGetKvs[0].Version
	}

	return ErrVersionConflict(resourcePointer, etcdVersion, foundVersion)
}

// Watch a resource.
func (st *State) Watch(ctx context.Context, resourcePointer resource.Pointer, ch chan<- state.Event, opts ...state.WatchOption) error {
	ctx = st.clearIncomingContext(ctx)

	var options state.WatchOptions

	for _, opt := range opts {
		opt(&options)
	}

	if options.TailEvents > 0 {
		return ErrUnsupported("tailEvents")
	}

	etcdKey := st.etcdKeyFromPointer(resourcePointer)

	var (
		curResource resource.Resource
		err         error
	)

	getResp, err := st.cli.Get(ctx, etcdKey)
	if err != nil {
		return err
	}

	var initialEvent state.Event

	if len(getResp.Kvs) > 0 {
		curResource, err = st.unmarshalResource(getResp.Kvs[0])
		if err != nil {
			return err
		}

		initialEvent.Resource = curResource
		initialEvent.Type = state.Created
	} else {
		initialEvent.Resource = resource.NewTombstone(
			resource.NewMetadata(
				resourcePointer.Namespace(),
				resourcePointer.Type(),
				resourcePointer.ID(),
				resource.VersionUndefined,
			))
		initialEvent.Type = state.Destroyed
	}

	revision := getResp.Header.Revision

	watchCh := st.cli.Watch(ctx, etcdKey, clientv3.WithPrevKV(), clientv3.WithRev(revision))

	go func() {
		select {
		case <-ctx.Done():
			return
		case ch <- initialEvent:
		}

		for {
			var watchResponse clientv3.WatchResponse

			select {
			case <-ctx.Done():
				return
			case watchResponse = <-watchCh:
			}

			if watchResponse.Canceled {
				return
			}

			for _, etcdEvent := range watchResponse.Events {
				event, err := st.convertEvent(etcdEvent)
				if err != nil {
					// skip the event
					continue
				}

				select {
				case <-ctx.Done():
					return
				case ch <- *event:
				}
			}
		}
	}()

	return nil
}

// WatchKind all resources by type.
//
//nolint:gocyclo,cyclop,gocognit
func (st *State) WatchKind(ctx context.Context, resourceKind resource.Kind, ch chan<- state.Event, opts ...state.WatchKindOption) error {
	ctx = st.clearIncomingContext(ctx)

	var options state.WatchKindOptions

	for _, opt := range opts {
		opt(&options)
	}

	matches := func(res resource.Resource) bool {
		return options.LabelQuery.Matches(*res.Metadata().Labels())
	}

	if options.TailEvents > 0 {
		return ErrUnsupported("tailEvents")
	}

	etcdKey := st.etcdKeyPrefixFromKind(resourceKind)

	var bootstrapList []resource.Resource

	var revision int64

	if options.BootstrapContents {
		getResp, err := st.cli.Get(ctx, etcdKey, clientv3.WithPrefix())
		if err != nil {
			return err
		}

		revision = getResp.Header.Revision

		for _, kv := range getResp.Kvs {
			res, err := st.unmarshalResource(kv)
			if err != nil {
				return err
			}

			if !matches(res) {
				continue
			}

			bootstrapList = append(bootstrapList, res)
		}

		sort.Slice(bootstrapList, func(i, j int) bool {
			return bootstrapList[i].Metadata().ID() < bootstrapList[j].Metadata().ID()
		})
	}

	watchCh := st.cli.Watch(ctx, etcdKey, clientv3.WithPrefix(), clientv3.WithPrevKV(), clientv3.WithRev(revision))

	go func() {
		sentBootstrapEventSet := make(map[string]struct{}, len(bootstrapList))

		// send initial contents if they were captured
		for _, res := range bootstrapList {
			select {
			case ch <- state.Event{
				Type:     state.Created,
				Resource: res,
			}:
			case <-ctx.Done():
				return
			}

			sentBootstrapEventSet[res.Metadata().String()] = struct{}{}
		}

		bootstrapList = nil

		for {
			var watchResponse clientv3.WatchResponse

			select {
			case <-ctx.Done():
				return
			case watchResponse = <-watchCh:
			}

			if watchResponse.Canceled {
				return
			}

			for _, etcdEvent := range watchResponse.Events {
				event, err := st.convertEvent(etcdEvent)
				if err != nil {
					// skip the event
					continue
				}

				switch event.Type {
				case state.Created, state.Destroyed:
					if !matches(event.Resource) {
						// skip the event
						continue
					}
				case state.Updated:
					oldMatches := matches(event.Old)
					newMatches := matches(event.Resource)

					switch {
					// transform the event if matching fact changes with the update
					case oldMatches && !newMatches:
						event.Type = state.Destroyed
						event.Old = nil
					case !oldMatches && newMatches:
						event.Type = state.Created
						event.Old = nil
					case newMatches && oldMatches:
						// passthrough the event
					default:
						// skip the event
						continue
					}
				}

				if !(event.Type == state.Destroyed) {
					_, alreadySent := sentBootstrapEventSet[event.Resource.Metadata().String()]
					if alreadySent {
						continue
					}
				}

				select {
				case <-ctx.Done():
					return
				case ch <- *event:
				}
			}
		}
	}()

	return nil
}

func (st *State) unmarshalResource(kv *mvccpb.KeyValue) (resource.Resource, error) {
	res, err := st.marshaler.UnmarshalResource(kv.Value)
	if err != nil {
		return nil, err
	}

	version, err := resource.ParseVersion(strconv.FormatInt(kv.Version, 10))

	res.Metadata().SetVersion(version)

	return res, err
}

func (st *State) convertEvent(etcdEvent *clientv3.Event) (*state.Event, error) {
	if etcdEvent == nil {
		return nil, fmt.Errorf("etcd event is nil")
	}

	var (
		current  resource.Resource
		previous resource.Resource
		err      error
	)

	if etcdEvent.Kv != nil && etcdEvent.Type != clientv3.EventTypeDelete {
		current, err = st.unmarshalResource(etcdEvent.Kv)
		if err != nil {
			return nil, err
		}
	}

	if etcdEvent.PrevKv != nil {
		previous, err = st.unmarshalResource(etcdEvent.PrevKv)
		if err != nil {
			return nil, err
		}
	}

	if etcdEvent.Type == clientv3.EventTypeDelete {
		return &state.Event{
			Resource: previous,
			Type:     state.Destroyed,
		}, nil
	}

	eventType := state.Updated
	if etcdEvent.IsCreate() {
		eventType = state.Created
	}

	return &state.Event{
		Resource: current,
		Old:      previous,
		Type:     eventType,
	}, nil
}

// clearIncomingContext returns a new context with the given parent context but with all incoming GRPC metadata removed.
//
// This is useful for preventing the GRPC metadata from being forwarded to etcd, e.g. in cases where an embedded etcd is used.
func (st *State) clearIncomingContext(ctx context.Context) context.Context {
	return util.ClearContextMeta(ctx)
}
