package handlers

import (
	"context"
	"net"
	"net/netip"

	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// checkSharePolicy applies the share's access policy (runtime.CheckShareAccess)
// to one tree of a session. It is the SMB mapping of that policy:
//
//   - proof: a guest or anonymous session carries none, a Kerberos session its
//     ticket, any other session the password NTLM verified;
//   - protection: what every request on the tree must carry. Privacy when each
//     must be encrypted, because the session encrypts or because the tree is an
//     encrypt_data one and the session holds the keys to meet it; integrity
//     when each must be signed. A session that cannot encrypt never reaches
//     privacy, so a krb5p share refuses it here rather than admitting a tree
//     whose every request then fails;
//   - netgroup: the address of the connection that carried TREE_CONNECT.
func (h *Handler) checkSharePolicy(ctx context.Context, share *runtime.Share, sess *session.Session, snap session.AuthzIdentity, encryptTree bool, clientAddr string) error {
	proof := runtime.ProofNTLM
	switch {
	case snap.IsGuest || snap.IsNull:
		proof = runtime.ProofNone
	case snap.Kerberos:
		proof = runtime.ProofKerberos
	}

	protection := runtime.ProtectionNone
	if cs := sess.GetCryptoState(); cs != nil {
		switch {
		case cs.ShouldEncrypt() || encryptTree && cs.Decryptor != nil:
			protection = runtime.ProtectionPrivacy
		case cs.SigningRequired:
			protection = runtime.ProtectionIntegrity
		}
	}

	// A share with no netgroup admits every address, so it needs no lookup.
	var clientIP net.IP
	var netgroup runtime.NetgroupChecker
	if share.NetgroupName != "" {
		netgroup = h.Registry.CheckNetgroupAccess
		if ap, err := netip.ParseAddrPort(clientAddr); err == nil {
			clientIP = ap.Addr().AsSlice()
		}
	}

	return runtime.CheckShareAccess(ctx, share, proof, protection, clientIP, netgroup)
}

// recheckTreePolicy re-applies the share's access policy to every tree of a
// session that has just re-authenticated. MS-SMB2 keeps tree connections
// across a re-authentication, and the new identity may hold a weaker proof than
// the one a tree was admitted on; a tree it no longer passes is removed, as the
// authorization re-check removes one.
//
// decision: the grant is not re-resolved here, only the policy. MS-SMB2 keeps a
// tree and its access across a re-authentication, and a client that
// re-authenticates as anonymous and back expects its trees to keep working; the
// next control-plane change re-resolves the grant, as for every session.
// Re-resolve it here too if a re-authentication must withdraw access at once.
func (h *Handler) recheckTreePolicy(ctx context.Context, sess *session.Session) {
	snap := sess.AuthzIdentity()
	var refused []uint32
	h.trees.Range(func(_, value any) bool {
		tree, ok := value.(*TreeConnection)
		if !ok || tree.SessionID != sess.SessionID {
			return true
		}
		// IPC$ has no share policy, and a share that is gone is the
		// authorization re-check's to retire.
		share, err := h.Registry.GetShare(tree.ShareName)
		if err != nil {
			return true
		}
		if err := h.checkSharePolicy(ctx, share, sess, snap, tree.EncryptData, tree.ClientAddr); err != nil {
			logger.Info("SMB tree refused by the share's access policy after re-authentication",
				"treeID", tree.TreeID, "share", share.Name, "sessionID", sess.SessionID, "reason", err)
			refused = append(refused, tree.TreeID)
		}
		return true
	})
	for _, treeID := range refused {
		h.removeTree(ctx, treeID, sess.SessionID)
	}
}
