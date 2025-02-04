// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package etcd provides an implementation of state.State in etcd.
package etcd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"sort"
	"strconv"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/impl/store"
	"github.com/siderolabs/gen/channel"
	"github.com/siderolabs/gen/xslices"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
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
		return nil, fmt.Errorf("etcd call failed on get %q: %w", resourcePointer, err)
	}

	if len(resp.Kvs) == 0 {
		return nil, fmt.Errorf("failed to get: %w", ErrNotFound(resourcePointer))
	}

	unmarshaled, err := st.unmarshalResource(resp.Kvs[0])
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal on get %q: %w", resourcePointer, err)
	}

	return unmarshaled, nil
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
		return resource.List{}, fmt.Errorf("etcd call failed on list %q: %w", resourceKind, err)
	}

	resources := make([]resource.Resource, 0, len(resp.Kvs))

	for _, kv := range resp.Kvs {
		res, err := st.unmarshalResource(kv)
		if err != nil {
			return resource.List{}, fmt.Errorf("failed to unmarshal on list %q: %w", resourceKind, err)
		}

		if !options.LabelQueries.Matches(*res.Metadata().Labels()) {
			continue
		}

		if !options.IDQuery.Matches(*res.Metadata()) {
			continue
		}

		resources = append(resources, res)
	}

	sort.Slice(resources, func(i, j int) bool {
		return resources[i].Metadata().ID() < resources[j].Metadata().ID()
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
		return fmt.Errorf("failed to set owner on create %q: %w", resCopy.Metadata(), err)
	}

	resCopy.Metadata().SetCreated(time.Now())

	data, err := st.marshaler.MarshalResource(resCopy)
	if err != nil {
		return fmt.Errorf("failed to marshal on create %q: %w", resCopy.Metadata(), err)
	}

	txnResp, err := st.cli.Txn(ctx).If(
		clientv3.Compare(clientv3.Version(etcdKey), "=", 0), // not exists check
	).Then(
		clientv3.OpPut(etcdKey, string(data)),
		clientv3.OpGet(etcdKey),
	).Commit()
	if err != nil {
		return fmt.Errorf("etcd call failed on create %q: %w", resCopy.Metadata(), err)
	}

	if !txnResp.Succeeded {
		return fmt.Errorf("failed to create: %w", ErrAlreadyExists(resCopy.Metadata()))
	}

	versionStr := strconv.FormatInt(txnResp.Responses[1].GetResponseRange().Kvs[0].Version, 10)

	version, err := resource.ParseVersion(versionStr)
	if err != nil {
		return fmt.Errorf("failed to parse version in etcd on create %q: %w", resCopy.Metadata(), err)
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
		return fmt.Errorf("etcd call failed on update %q: %w", resCopy.Metadata(), err)
	}

	if len(getResp.Kvs) == 0 {
		return fmt.Errorf("failed to update: %w", ErrNotFound(resCopy.Metadata()))
	}

	curResource, err := st.unmarshalResource(getResp.Kvs[0])
	if err != nil {
		return fmt.Errorf("failed to unmarshal on update %q: %w", resCopy.Metadata(), err)
	}

	updated := time.Now()

	resCopy.Metadata().SetUpdated(updated)
	resCopy.Metadata().SetCreated(curResource.Metadata().Created())

	data, err := st.marshaler.MarshalResource(resCopy)
	if err != nil {
		return fmt.Errorf("failed to marshal on update %q: %w", resCopy.Metadata(), err)
	}

	expectedVersion := int64(resCopy.Metadata().Version().Value())

	etcdVersion := getResp.Kvs[0].Version

	if etcdVersion != expectedVersion {
		return fmt.Errorf("failed to update: %w", ErrVersionConflict(curResource.Metadata(), expectedVersion, etcdVersion))
	}

	if curResource.Metadata().Owner() != options.Owner {
		return fmt.Errorf("failed to update: %w", ErrOwnerConflict(curResource.Metadata(), curResource.Metadata().Owner()))
	}

	if options.ExpectedPhase != nil && curResource.Metadata().Phase() != *options.ExpectedPhase {
		return fmt.Errorf("failed to update: %w", ErrPhaseConflict(curResource.Metadata(), *options.ExpectedPhase))
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
		return fmt.Errorf("etcd call failed on update %q: %w", resCopy.Metadata(), err)
	}

	if !txnResp.Succeeded {
		var foundVersion int64

		txnGetKvs := txnResp.Responses[0].GetResponseRange().Kvs
		if len(txnResp.Responses[0].GetResponseRange().Kvs) > 0 {
			foundVersion = txnGetKvs[0].Version
		}

		return fmt.Errorf("failed to update: %w", ErrVersionConflict(resCopy.Metadata(), expectedVersion, foundVersion))
	}

	versionStr := strconv.FormatInt(txnResp.Responses[1].GetResponseRange().Kvs[0].Version, 10)

	version, err := resource.ParseVersion(versionStr)
	if err != nil {
		return fmt.Errorf("failed to parse version in etcd on update %q: %w", resCopy.Metadata(), err)
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
		return fmt.Errorf("etcd call failed on destroy %q: %w", resourcePointer, err)
	}

	if len(resp.Kvs) == 0 {
		return fmt.Errorf("failed to destroy: %w", ErrNotFound(resourcePointer))
	}

	etcdVersion := resp.Kvs[0].Version

	curResource, err := st.unmarshalResource(resp.Kvs[0])
	if err != nil {
		return fmt.Errorf("failed to unmarshal on destroy %q: %w", resourcePointer, err)
	}

	if curResource.Metadata().Owner() != options.Owner {
		return fmt.Errorf("failed to destroy: %w", ErrOwnerConflict(curResource.Metadata(), curResource.Metadata().Owner()))
	}

	if !curResource.Metadata().Finalizers().Empty() {
		return fmt.Errorf("failed to destroy: %w", ErrPendingFinalizers(*curResource.Metadata()))
	}

	txnResp, err := st.cli.Txn(ctx).If(
		clientv3.Compare(clientv3.Version(etcdKey), "=", etcdVersion),
	).Then(
		clientv3.OpDelete(etcdKey),
	).Else(
		clientv3.OpGet(etcdKey),
	).Commit()
	if err != nil {
		return fmt.Errorf("etcd call failed on destroy %q: %w", resourcePointer, err)
	}

	if txnResp.Succeeded {
		return nil
	}

	var foundVersion int64

	txnGetKvs := txnResp.Responses[0].GetResponseRange().Kvs
	if len(txnGetKvs) == 0 {
		return nil
	}

	foundVersion = txnGetKvs[0].Version

	return fmt.Errorf("failed to destroy: %w", ErrVersionConflict(resourcePointer, etcdVersion, foundVersion))
}

func encodeBookmark(revision int64) state.Bookmark {
	return binary.BigEndian.AppendUint64(nil, uint64(revision))
}

func decodeBookmark(bookmark state.Bookmark) (int64, error) {
	if len(bookmark) != 8 {
		return 0, ErrInvalidWatchBookmark(fmt.Errorf("invalid bookmark length: %d", len(bookmark)))
	}

	return int64(binary.BigEndian.Uint64(bookmark)), nil
}

// Watch a resource.
//
//nolint:gocognit,gocyclo,cyclop
func (st *State) Watch(ctx context.Context, resourcePointer resource.Pointer, ch chan<- state.Event, opts ...state.WatchOption) error {
	ctx = st.clearIncomingContext(ctx)

	var options state.WatchOptions

	for _, opt := range opts {
		opt(&options)
	}

	etcdKey := st.etcdKeyFromPointer(resourcePointer)

	var (
		revision     int64
		initialEvent state.Event
	)

	switch {
	case options.TailEvents > 0:
		return fmt.Errorf("failed to watch: %w", ErrUnsupported("tailEvents"))
	case options.StartFromBookmark != nil:
		var err error

		revision, err = decodeBookmark(options.StartFromBookmark)
		if err != nil {
			return fmt.Errorf("failed to watch %q: %w", resourcePointer, err)
		}
	default:
		var curResource resource.Resource

		getResp, err := st.cli.Get(ctx, etcdKey)
		if err != nil {
			return fmt.Errorf("etcd call failed on watch %q: %w", resourcePointer, err)
		}

		revision = getResp.Header.Revision
		initialEvent.Bookmark = encodeBookmark(getResp.Header.Revision)

		if len(getResp.Kvs) > 0 {
			curResource, err = st.unmarshalResource(getResp.Kvs[0])
			if err != nil {
				return fmt.Errorf("failed to unmarshal on watch %q: %w", resourcePointer, err)
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
	}

	// wrap the context to make sure Watch is aborted if the loop terminates
	ctx, cancel := context.WithCancel(ctx)
	ctx = clientv3.WithRequireLeader(ctx)

	watchCh := st.cli.Watch(ctx, etcdKey, clientv3.WithPrevKV(), clientv3.WithRev(revision))

	go func() {
		defer func() {
			// drain the watchCh, etcd Watch API guarantees that the channel is closed when the watcher is canceled
			for range watchCh { //nolint:revive
			}
		}()
		defer cancel()

		if initialEvent.Resource != nil {
			if !channel.SendWithContext(ctx, ch, initialEvent) {
				return
			}
		}

		for watchResponse := range chanItems(ctx, watchCh) {
			if watchResponse.Err() != nil {
				err := watchResponse.Err()

				switch {
				case errors.Is(err, rpctypes.ErrCompacted):
					err = ErrInvalidWatchBookmark(err)
				case errors.Is(err, rpctypes.ErrFutureRev):
					err = ErrInvalidWatchBookmark(err)
				}

				channel.SendWithContext(ctx, ch,
					state.Event{
						Type:  state.Errored,
						Error: err,
					},
				)

				return
			}

			if watchResponse.Canceled {
				return
			}

			for _, etcdEvent := range watchResponse.Events {
				if etcdEvent.Kv != nil && etcdEvent.Kv.ModRevision <= revision {
					continue
				}

				event, err := st.convertEvent(etcdEvent)
				if err != nil {
					channel.SendWithContext(ctx, ch,
						state.Event{
							Type:  state.Errored,
							Error: err,
						},
					)

					return
				}

				if !channel.SendWithContext(ctx, ch, event) {
					return
				}
			}
		}
	}()

	return nil
}

// WatchKind all resources by type.
func (st *State) WatchKind(ctx context.Context, resourceKind resource.Kind, ch chan<- state.Event, opts ...state.WatchKindOption) error {
	return st.watchKind(ctx, resourceKind, ch, nil, "watchKind", opts...)
}

// WatchKindAggregated all resources by type.
func (st *State) WatchKindAggregated(ctx context.Context, resourceKind resource.Kind, ch chan<- []state.Event, opts ...state.WatchKindOption) error {
	return st.watchKind(ctx, resourceKind, nil, ch, "watchKindAggregated", opts...)
}

//nolint:gocyclo,cyclop,gocognit,maintidx
func (st *State) watchKind(ctx context.Context, resourceKind resource.Kind, singleCh chan<- state.Event, aggCh chan<- []state.Event, opName string, opts ...state.WatchKindOption) error {
	ctx = st.clearIncomingContext(ctx)

	var options state.WatchKindOptions

	for _, opt := range opts {
		opt(&options)
	}

	matches := func(res resource.Resource) bool {
		return options.LabelQueries.Matches(*res.Metadata().Labels()) && options.IDQuery.Matches(*res.Metadata())
	}

	etcdKey := st.etcdKeyPrefixFromKind(resourceKind)

	var (
		bootstrapList []resource.Resource
		revision      int64
	)

	switch {
	case options.TailEvents > 0:
		return fmt.Errorf("failed to %s: %w", opName, ErrUnsupported("tailEvents"))
	case options.StartFromBookmark != nil && options.BootstrapContents:
		return fmt.Errorf("failed to %s: %w", opName, ErrUnsupported("startFromBookmark and bootstrapContents"))
	case options.StartFromBookmark != nil:
		var err error

		revision, err = decodeBookmark(options.StartFromBookmark)
		if err != nil {
			return fmt.Errorf("failed to %s %q: %w", opName, resourceKind, err)
		}
	case options.BootstrapContents:
		getResp, err := st.cli.Get(ctx, etcdKey, clientv3.WithPrefix())
		if err != nil {
			return fmt.Errorf("etcd call failed on %s %q: %w", opName, resourceKind, err)
		}

		revision = getResp.Header.Revision

		for _, kv := range getResp.Kvs {
			res, err := st.unmarshalResource(kv)
			if err != nil {
				return fmt.Errorf("failed to unmarshal on %s %q: %w", opName, resourceKind, err)
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

	if !options.BootstrapContents && options.BootstrapBookmark {
		// fetch initial revision
		getResp, err := st.cli.Get(ctx, etcdKey)
		if err != nil {
			return fmt.Errorf("etcd call failed on %s %q: %w", opName, resourceKind, err)
		}

		revision = getResp.Header.Revision
	}

	// wrap the context to make sure Watch is aborted if the loop terminates
	ctx, cancel := context.WithCancel(ctx)
	ctx = clientv3.WithRequireLeader(ctx)

	watchCh := st.cli.Watch(ctx, etcdKey, clientv3.WithPrefix(), clientv3.WithPrevKV(), clientv3.WithRev(revision))

	go func() {
		defer func() {
			cancel()

			// drain the watchCh, etcd Watch API guarantees that the channel is closed when the watcher is canceled
			for range watchCh { //nolint:revive
			}
		}()

		if options.BootstrapContents {
			switch {
			case singleCh != nil:
				for _, res := range bootstrapList {
					if !channel.SendWithContext(ctx, singleCh,
						state.Event{
							Type:     state.Created,
							Resource: res,
						},
					) {
						return
					}
				}

				if !channel.SendWithContext(
					ctx, singleCh,
					state.Event{
						Type:     state.Bootstrapped,
						Resource: resource.NewTombstone(resource.NewMetadata(resourceKind.Namespace(), resourceKind.Type(), "", resource.VersionUndefined)),
						Bookmark: encodeBookmark(revision),
					},
				) {
					return
				}
			case aggCh != nil:
				events := xslices.Map(bootstrapList, func(r resource.Resource) state.Event {
					return state.Event{
						Type:     state.Created,
						Resource: r,
					}
				})

				events = append(events, state.Event{
					Type:     state.Bootstrapped,
					Resource: resource.NewTombstone(resource.NewMetadata(resourceKind.Namespace(), resourceKind.Type(), "", resource.VersionUndefined)),
					Bookmark: encodeBookmark(revision),
				})

				if !channel.SendWithContext(ctx, aggCh, events) {
					return
				}
			}

			// make the list nil so that it gets GC'ed, we don't need it anymore after this point
			bootstrapList = nil
		}

		if options.BootstrapBookmark {
			event := state.Event{
				Type:     state.Noop,
				Resource: resource.NewTombstone(resource.NewMetadata(resourceKind.Namespace(), resourceKind.Type(), "", resource.VersionUndefined)),
				Bookmark: encodeBookmark(revision),
			}

			switch {
			case singleCh != nil:
				if !channel.SendWithContext(ctx, singleCh, event) {
					return
				}
			case aggCh != nil:
				if !channel.SendWithContext(ctx, aggCh, []state.Event{event}) {
					return
				}
			}
		}

		for watchResponse := range chanItems(ctx, watchCh) {
			if watchResponse.Err() != nil {
				err := watchResponse.Err()

				switch {
				case errors.Is(err, rpctypes.ErrCompacted):
					err = ErrInvalidWatchBookmark(err)
				case errors.Is(err, rpctypes.ErrFutureRev):
					err = ErrInvalidWatchBookmark(err)
				}

				watchErrorEvent := state.Event{
					Type:  state.Errored,
					Error: err,
				}

				switch {
				case singleCh != nil:
					channel.SendWithContext(ctx, singleCh, watchErrorEvent)
				case aggCh != nil:
					channel.SendWithContext(ctx, aggCh, []state.Event{watchErrorEvent})
				}

				return
			}

			if watchResponse.Canceled {
				return
			}

			events := make([]state.Event, 0, len(watchResponse.Events))

			for _, etcdEvent := range watchResponse.Events {
				// watch event might come for a revision which was already sent in the bootstrapped set, ignore it
				if etcdEvent.Kv != nil && etcdEvent.Kv.ModRevision <= revision {
					continue
				}

				event, err := st.convertEvent(etcdEvent)
				if err != nil {
					convertErrorEvent := state.Event{
						Type:  state.Errored,
						Error: err,
					}

					switch {
					case singleCh != nil:
						channel.SendWithContext(ctx, singleCh, convertErrorEvent)
					case aggCh != nil:
						channel.SendWithContext(ctx, aggCh, []state.Event{convertErrorEvent})
					}

					return
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
				case state.Errored, state.Bootstrapped, state.Noop:
					panic("should never be reached")
				}

				events = append(events, event)
			}

			if len(events) == 0 {
				continue
			}

			switch {
			case aggCh != nil:
				if !channel.SendWithContext(ctx, aggCh, events) {
					return
				}
			case singleCh != nil:
				for _, event := range events {
					if !channel.SendWithContext(ctx, singleCh, event) {
						return
					}
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

func (st *State) convertEvent(etcdEvent *clientv3.Event) (state.Event, error) {
	if etcdEvent == nil {
		return state.Event{}, fmt.Errorf("etcd event is nil")
	}

	var (
		current  resource.Resource
		previous resource.Resource
		err      error
	)

	if etcdEvent.Kv != nil && etcdEvent.Type != clientv3.EventTypeDelete {
		current, err = st.unmarshalResource(etcdEvent.Kv)
		if err != nil {
			return state.Event{}, err
		}
	}

	if etcdEvent.PrevKv != nil {
		previous, err = st.unmarshalResource(etcdEvent.PrevKv)
		if err != nil {
			return state.Event{}, err
		}
	}

	if etcdEvent.Type == clientv3.EventTypeDelete {
		return state.Event{
			Resource: previous,
			Type:     state.Destroyed,
			Bookmark: encodeBookmark(etcdEvent.Kv.ModRevision),
		}, nil
	}

	eventType := state.Updated
	bookmark := encodeBookmark(etcdEvent.Kv.ModRevision)

	if etcdEvent.IsCreate() {
		eventType = state.Created
		bookmark = encodeBookmark(etcdEvent.Kv.CreateRevision)
	}

	return state.Event{
		Resource: current,
		Old:      previous,
		Type:     eventType,
		Bookmark: bookmark,
	}, nil
}

// clearIncomingContext returns a new context with the given parent context but with all incoming GRPC metadata removed.
//
// This is useful for preventing the GRPC metadata from being forwarded to etcd, e.g. in cases where an embedded etcd is used.
func (st *State) clearIncomingContext(ctx context.Context) context.Context {
	return util.ClearContextMeta(ctx)
}

func chanItems[T any](ctx context.Context, ch <-chan T) iter.Seq[T] {
	return func(yield func(T) bool) {
		for {
			select {
			case <-ctx.Done():
			case item, ok := <-ch:
				if ok && yield(item) {
					continue
				}
			}

			return
		}
	}
}
