package openstate

import (
	"errors"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// The filesystem service maps each of these once to its own neutral errors.
var (
	ErrBadState      = errors.New("openstate: stateid names no record")
	ErrStaleState    = errors.New("openstate: state lost with its shard's open state")
	ErrNotYours      = errors.New("openstate: state held by another client")
	ErrStaleClient   = errors.New("openstate: client expired, or never confirmed here")
	ErrClientInUse   = errors.New("openstate: owner held by another principal")
	ErrBadOwner      = errors.New("openstate: owner empty or longer than 1,024 bytes")
	ErrDelay         = errors.New("openstate: retry later")
	ErrSeqMisordered = entity.ErrSeqMisordered
)
