// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package etcd

import (
	"fmt"

	"github.com/cosi-project/runtime/pkg/resource"
)

type eNotFound struct {
	error
}

func (eNotFound) NotFoundError() {}

// ErrNotFound generates error compatible with state.ErrNotFound.
func ErrNotFound(r resource.Pointer) error {
	return eNotFound{
		fmt.Errorf("resource %s doesn't exist", r),
	}
}

type eConflict struct {
	error
}

func (eConflict) ConflictError() {}

type eOwnerConflict struct {
	eConflict
}

func (eOwnerConflict) OwnerConflictError() {}

type ePhaseConflict struct {
	eConflict
}

func (ePhaseConflict) PhaseConflictError() {}

type eUnsupported struct {
	error
}

func (eUnsupported) UnsupportedError() {}

// ErrAlreadyExists generates error compatible with state.ErrConflict.
func ErrAlreadyExists(r resource.Reference) error {
	return eConflict{
		fmt.Errorf("resource %s already exists", r),
	}
}

// ErrVersionConflict generates error compatible with state.ErrConflict.
func ErrVersionConflict(r resource.Pointer, expected, found int64) error {
	return eConflict{
		fmt.Errorf("resource %s update conflict: expected version %q, actual version %q", r, expected, found),
	}
}

// ErrPendingFinalizers generates error compatible with state.ErrConflict.
func ErrPendingFinalizers(r resource.Metadata) error {
	return eConflict{
		fmt.Errorf("resource %s has pending finalizers %s", r, r.Finalizers()),
	}
}

// ErrOwnerConflict generates error compatible with state.ErrConflict.
func ErrOwnerConflict(r resource.Reference, owner string) error {
	return eOwnerConflict{
		eConflict{
			fmt.Errorf("resource %s is owned by %q", r, owner),
		},
	}
}

// ErrPhaseConflict generates error compatible with ErrConflict.
func ErrPhaseConflict(r resource.Reference, expectedPhase resource.Phase) error {
	return ePhaseConflict{
		eConflict{
			fmt.Errorf("resource %s is not in phase %s", r, expectedPhase),
		},
	}
}

// ErrUnsupported generates error compatible with state.ErrUnsupported.
func ErrUnsupported(operation string) error {
	return eUnsupported{
		fmt.Errorf("operation %s is not supported", operation),
	}
}
