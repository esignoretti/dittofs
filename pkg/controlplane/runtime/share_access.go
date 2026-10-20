package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ErrShareAccessDenied is returned by CheckShareAccess when a caller fails the
// share's access policy. Every denial wraps it, with the rule that fired in the
// message, so each protocol can map the one decision onto its own status and
// log why.
var ErrShareAccessDenied = errors.New("share access denied")

// Proof is what a caller's authentication verified, weakest first.
type Proof uint8

const (
	// ProofNone is a caller nothing verified: NFS AUTH_SYS and AUTH_NULL, an
	// SMB guest or anonymous session.
	ProofNone Proof = iota
	// ProofNTLM is an SMB session that proved its password with NTLM.
	ProofNTLM
	// ProofKerberos is a caller holding a verified Kerberos context.
	ProofKerberos
)

// Protection is what protects every message a caller sends, weakest first.
type Protection uint8

const (
	// ProtectionNone leaves messages in clear.
	ProtectionNone Protection = iota
	// ProtectionIntegrity signs every message.
	ProtectionIntegrity
	// ProtectionPrivacy encrypts every message.
	ProtectionPrivacy
)

// NetgroupChecker reports whether a client address is allowed by a share's
// netgroup allowlist. Passing the lookup in keeps the policy dependent on the
// one call it needs rather than on the whole control plane;
// *Runtime.CheckNetgroupAccess satisfies it.
type NetgroupChecker func(ctx context.Context, shareName string, clientIP net.IP) (bool, error)

// CheckShareAccess applies a share's access policy to a caller, the part every
// protocol shares: which proofs of identity the share accepts, the protection
// an authenticated caller's messages must carry, and which client addresses may
// reach it. Each protocol maps its own credential onto proof and protection. It
// returns nil when the caller may proceed and an error wrapping
// ErrShareAccessDenied otherwise. share must not be nil.
//
// AllowAuthSys=false refuses a caller nothing verified, and RequireKerberos
// refuses every proof but Kerberos.
//
// The netgroup allowlist gates the client address, and only when a lookup is
// supplied: a nil netgroup means the caller applies no address gate here (an
// empty allowlist already means "allow all"). When one is supplied, an
// unparseable client address and a failed lookup both deny.
func CheckShareAccess(
	ctx context.Context,
	share *Share,
	proof Proof,
	protection Protection,
	clientIP net.IP,
	netgroup NetgroupChecker,
) error {
	if proof == ProofNone && !share.AllowAuthSys {
		return fmt.Errorf("%w: share %q accepts only authenticated callers",
			ErrShareAccessDenied, share.Name)
	}
	if proof != ProofKerberos && share.RequireKerberos {
		return fmt.Errorf("%w: share %q requires Kerberos",
			ErrShareAccessDenied, share.Name)
	}
	// decision: min_kerberos_level is the protection floor for any
	// authenticated caller, NTLM included, although it is named for Kerberos: a
	// share that refuses a Kerberos session in clear must not admit the weaker
	// proof in clear. A caller nothing verified holds no key to protect a
	// message with, so it stays under AllowAuthSys alone, as AUTH_SYS always
	// has on NFS. Narrow the floor to Kerberos only if a client that cannot
	// sign is shown to need such a share.
	if proof != ProofNone && !MeetsMinKerberosLevel(share.MinKerberosLevel, protection) {
		return fmt.Errorf("%w: share %q requires min kerberos level %q (protection %d)",
			ErrShareAccessDenied, share.Name, share.MinKerberosLevel, protection)
	}

	if netgroup == nil {
		return nil
	}
	if clientIP == nil {
		return fmt.Errorf("%w: client address for share %q is not an IP, cannot evaluate the netgroup allowlist",
			ErrShareAccessDenied, share.Name)
	}
	allowed, err := netgroup(ctx, share.Name, clientIP)
	if err != nil {
		return fmt.Errorf("%w: netgroup check for share %q failed: %w",
			ErrShareAccessDenied, share.Name, err)
	}
	if !allowed {
		return fmt.Errorf("%w: client %s is not in the netgroup allowed for share %q",
			ErrShareAccessDenied, clientIP, share.Name)
	}
	return nil
}

// MeetsMinKerberosLevel reports whether a caller whose messages carry
// protection satisfies a min_kerberos_level floor: krb5 asks for
// authentication only, krb5i for integrity and krb5p for privacy. An empty or
// unrecognised level sets no floor.
func MeetsMinKerberosLevel(minLevel string, protection Protection) bool {
	switch minLevel {
	case models.KerberosLevelKrb5i:
		return protection >= ProtectionIntegrity
	case models.KerberosLevelKrb5p:
		return protection >= ProtectionPrivacy
	default:
		return true
	}
}
