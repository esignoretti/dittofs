package auth

import (
	"context"
	"fmt"
	"net"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// ErrExportAccessDenied is returned by CheckExportAccess when a request fails
// the share's export access-control policy. Every denial wraps it, with the
// rule that fired in the message, so a caller can map one decision onto its own
// protocol status (MNT3ERR_ACCES, NFS3ERR_ACCES, NFS4ERR_WRONGSEC) and log why.
// It is the shared policy's error, so SMB and NFS refuse with one value.
var ErrExportAccessDenied = runtime.ErrShareAccessDenied

// NetgroupChecker reports whether a client address is allowed by a share's
// netgroup allowlist; *runtime.Runtime.CheckNetgroupAccess satisfies it.
type NetgroupChecker = runtime.NetgroupChecker

// CheckExportAccess applies a share's access policy (runtime.CheckShareAccess)
// to one NFS request: which auth flavors the export accepts, the protection
// level a Kerberos session must have negotiated, and which client addresses may
// reach it. It is the NFS mapping of that policy: AUTH_SYS and AUTH_NONE carry
// no verified proof, and RPCSEC_GSS carries Kerberos at the service level it
// negotiated. It returns nil when the request may proceed and an error wrapping
// ErrExportAccessDenied otherwise. share must not be nil.
//
// Both flavor gates test for RPCSEC_GSS rather than against AUTH_UNIX: a share
// that refuses AUTH_SYS refuses everything weaker than it too, and naming
// AUTH_UNIX alone would let an AUTH_NONE caller — no credential at all — past
// the gate.
//
// The negotiated GSS service level rides in ctx, attached by the GSS DATA
// dispatch to every RPCSEC_GSS request it processes — control messages and
// failures are answered there and never reach a caller of this function. So a
// claimed GSS flavor with no session info was never processed as GSS, which
// happens when no GSS processor is configured and the dispatch leaves flavor 6
// unintercepted. That credential is unverified and cannot stand in for
// Kerberos — RequireKerberos is satisfied by the flavor alone and AllowAuthSys
// does not apply to it — so it is denied, not skipped.
//
// The netgroup allowlist gates the client address, and only when a lookup is
// supplied: a nil netgroup means the caller applies no address gate here (an
// empty allowlist already means "allow all"). When one is supplied, an
// unparseable client address and a failed lookup both deny.
func CheckExportAccess(
	ctx context.Context,
	share *runtime.Share,
	authFlavor uint32,
	clientIP net.IP,
	netgroup NetgroupChecker,
) error {
	proof, protection := runtime.ProofNone, runtime.ProtectionNone
	if authFlavor == rpc.AuthRPCSECGSS {
		si := gss.SessionInfoFromContext(ctx)
		if si == nil {
			return fmt.Errorf("%w: RPCSEC_GSS credential for share %q was not verified",
				ErrExportAccessDenied, share.Name)
		}
		proof, protection = runtime.ProofKerberos, gssProtection(si.Service)
	}
	if err := runtime.CheckShareAccess(ctx, share, proof, protection, clientIP, netgroup); err != nil {
		return fmt.Errorf("auth flavor %d: %w", authFlavor, err)
	}
	return nil
}
