// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package util contains utility functions for the project.
package util

import (
	"context"

	"google.golang.org/grpc/metadata"
)

// ClearContextMeta returns a new context with the given parent context but with all incoming GRPC metadata removed.
//
// This is useful for preventing the GRPC metadata from being forwarded to etcd, e.g. in cases where an embedded etcd is used.
func ClearContextMeta(ctx context.Context) context.Context {
	return metadata.NewIncomingContext(ctx, metadata.MD{})
}
