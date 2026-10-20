package auth

import (
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// MeetsMinKerberosLevel reports whether an RPCSEC_GSS request that negotiated
// the `negotiated` service level (one of gss.RPCGSSSvc*) satisfies the share's
// configured min_kerberos_level floor. An empty or unrecognised
// min_kerberos_level means no floor is configured and the request is allowed.
//
// This is the wire-protection counterpart to RequireKerberos: RequireKerberos
// rejects non-GSS auth flavors, MeetsMinKerberosLevel rejects GSS sessions
// negotiated below the requested protection level (e.g. a plain `krb5`
// authentication-only session on a share that demands `krb5p` privacy). It is
// only meaningful for RPCSEC_GSS requests; non-GSS flavors are governed by
// AllowAuthSys / RequireKerberos.
func MeetsMinKerberosLevel(minLevel string, negotiated uint32) bool {
	return runtime.MeetsMinKerberosLevel(minLevel, gssProtection(negotiated))
}

// gssProtection maps an RPCSEC_GSS service level onto the protection it gives
// every message: none(1) authenticates only, integrity(2) signs, privacy(3)
// encrypts. Anything else protects nothing.
func gssProtection(service uint32) runtime.Protection {
	switch service {
	case gss.RPCGSSSvcIntegrity:
		return runtime.ProtectionIntegrity
	case gss.RPCGSSSvcPrivacy:
		return runtime.ProtectionPrivacy
	default:
		return runtime.ProtectionNone
	}
}
