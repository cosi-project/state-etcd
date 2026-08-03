// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package etcd

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"

	"github.com/cosi-project/runtime/pkg/resource"
)

// etcdKeyFromPointer builds a key for the resource with the given pointer.
func (st *State) etcdKeyFromPointer(pointer resource.Pointer) string {
	prefix := st.etcdKeyPrefixFromKind(pointer)

	hashedID := sha256hex(append([]byte(pointer.ID()), st.salt...))

	return prefix + hashedID
}

// etcdKeyPrefixFromKind returns a key prefix for the given resource kind.
func (st *State) etcdKeyPrefixFromKind(kind resource.Kind) string {
	nsEscaped := url.PathEscape(kind.Namespace())
	typeEscaped := url.PathEscape(kind.Type())

	return st.keyPrefix + "/" + nsEscaped + "/" + typeEscaped + "/"
}

// etcdKeyPrefixFromKey returns the kind prefix of the given etcd key.
//
// Keys are built as "<keyPrefix>/<namespace>/<type>/<hashedID>" by etcdKeyFromPointer, and the
// hashed ID is hex-encoded, so it never contains a slash: cutting at the last slash yields exactly
// the prefix etcdKeyPrefixFromKind would produce for the same kind.
func etcdKeyPrefixFromKey(key string) string {
	idx := strings.LastIndexByte(key, '/')
	if idx < 0 {
		return key
	}

	return key[:idx+1]
}

func sha256hex(input []byte) string {
	hash := sha256.Sum256(input)

	return hex.EncodeToString(hash[:])
}
