// Typed errors for share lifecycle operations.

package shares

import (
	"errors"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ErrShareAlreadyDisabled is returned when DisableShare is called on a
// share whose DB row already has enabled=false. Callers may treat this
// as a benign no-op or surface it (Phase-5 restore treats as OK).
var ErrShareAlreadyDisabled = errors.New("share is already disabled")

// ErrShareNotFound re-exports models.ErrShareNotFound to preserve
// errors.Is matching across package boundaries.
var ErrShareNotFound = models.ErrShareNotFound

// ErrShuttingDown is returned by the share operations that would install a
// running block store after CloseBlockStores has closed them all. The REST
// handlers already log an AddShare failure and leave the persisted row for the
// next boot to load, so refusing during shutdown is behaviour they are written
// for.
var ErrShuttingDown = errors.New("shares service is shutting down")

// ErrShareBusy is returned when a share name is claimed by an add or a removal
// still in flight, so a second one for the same name cannot overlap it.
var ErrShareBusy = errors.New("share is being added or removed")
