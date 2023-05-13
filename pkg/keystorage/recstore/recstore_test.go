// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package recstore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/siderolabs/gen/xerrors"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/cosi-project/state-etcd/pkg/keystorage/recstore"
	"github.com/cosi-project/state-etcd/pkg/util/testhelpers"
)

var hx = func() []byte {
	res := sha256.Sum256([]byte("expected"))

	return res[:]
}()

type data struct {
	binaryData []byte
}

func marshal(d *data) ([]byte, error) {
	return d.binaryData, nil
}

func unmarshal(b []byte) (*data, error) {
	if !bytes.Equal(b, hx) {
		return nil, fmt.Errorf("fail to unmarshal data")
	}

	return &data{binaryData: b}, nil
}

func TestRecStore(t *testing.T) {
	testhelpers.WithEtcd(t, func(client *clientv3.Client) {
		st := recstore.New(client, "instance-name", "record-prefix", marshal, unmarshal)

		_, err := st.Get(context.Background())
		errorShouldBe[recstore.NotFoundTag](t, err)

		err = st.Update(context.Background(), &data{binaryData: hx}, 1)
		errorShouldBe[recstore.NotFoundTag](t, err)

		err = st.Update(context.Background(), &data{binaryData: hx}, 0)
		require.NoError(t, err)

		err = st.Create(context.Background(), &data{binaryData: hx})
		errorShouldBe[recstore.AlreadyExistsTag](t, err)

		err = st.Update(context.Background(), &data{binaryData: hx}, 1)
		require.NoError(t, err)

		err = st.Update(context.Background(), &data{binaryData: hx}, 2)
		require.NoError(t, err)

		err = st.Update(context.Background(), &data{binaryData: hx}, 2)
		errorShouldBe[recstore.VersionConflictTag](t, err)

		got, err := st.Get(context.Background())
		require.NoError(t, err)

		require.EqualValues(t, 3, got.Version)

		err = st.Update(context.Background(), &data{binaryData: []byte("not-expected")}, 3)
		require.NoError(t, err)

		got, err = st.Get(context.Background())
		require.Zero(t, got)
		require.Regexp(t, "error unmarshaling data for .* fail to unmarshal data", err)
	})
}

func errorShouldBe[T xerrors.Tag](t *testing.T, err error) {
	require.True(t, xerrors.TagIs[T](err))
}
