package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
)

// TestCheckSharePolicy covers the SMB mapping of the share's access policy:
// which proof a session carries, and what protection the requests on a tree
// carry. Each case is one line of that mapping.
func TestCheckSharePolicy(t *testing.T) {
	user := &models.User{Username: "alice", Enabled: true}
	guest := func() *session.Session { return session.NewSession(1, "", true, "guest", "") }
	anonymous := func() *session.Session { return session.NewSession(1, "", false, "", "") }
	ntlm := func() *session.Session { return session.NewSessionWithUser(1, "", user, "") }
	kerberos := func() *session.Session {
		s := ntlm()
		s.SetExpiry(time.Now().Add(time.Hour))
		return s
	}
	signing := func(s *session.Session) *session.Session {
		s.GetCryptoState().SigningRequired = true
		return s
	}
	// canEncrypt holds the keys an encrypt_data tree needs, without the session
	// encrypting by itself.
	canEncrypt := func(s *session.Session) *session.Session {
		s.GetCryptoState().Decryptor = &mockEncryptor{}
		return s
	}
	encrypting := func(s *session.Session) *session.Session {
		cs := s.GetCryptoState()
		cs.EncryptData, cs.Encryptor, cs.Decryptor = true, &mockEncryptor{}, &mockEncryptor{}
		return s
	}

	open := runtime.Share{AllowAuthSys: true, MinKerberosLevel: models.KerberosLevelKrb5}
	noAuthSys := runtime.Share{AllowAuthSys: false}
	kerberosOnly := runtime.Share{AllowAuthSys: true, RequireKerberos: true}
	krb5i := runtime.Share{AllowAuthSys: true, MinKerberosLevel: models.KerberosLevelKrb5i}
	krb5p := runtime.Share{AllowAuthSys: true, MinKerberosLevel: models.KerberosLevelKrb5p}

	tests := []struct {
		name        string
		share       runtime.Share
		sess        *session.Session
		encryptTree bool
		wantDeny    bool
	}{
		{name: "the default policy admits a guest", share: open, sess: guest()},
		{name: "allow_auth_sys=false refuses a guest", share: noAuthSys, sess: guest(), wantDeny: true},
		{name: "allow_auth_sys=false refuses an anonymous session", share: noAuthSys, sess: anonymous(), wantDeny: true},
		{name: "allow_auth_sys=false admits NTLM", share: noAuthSys, sess: ntlm()},
		{name: "require_kerberos refuses NTLM", share: kerberosOnly, sess: ntlm(), wantDeny: true},
		{name: "require_kerberos refuses a guest", share: kerberosOnly, sess: guest(), wantDeny: true},
		{name: "require_kerberos admits Kerberos", share: kerberosOnly, sess: kerberos()},
		{name: "krb5i refuses an NTLM session that does not sign", share: krb5i, sess: ntlm(), wantDeny: true},
		{name: "krb5i admits a session that signs", share: krb5i, sess: signing(ntlm())},
		{name: "krb5i leaves a guest to allow_auth_sys", share: krb5i, sess: guest()},
		{name: "krb5p refuses a session that only signs", share: krb5p, sess: signing(kerberos()), wantDeny: true},
		{name: "krb5p admits a session that encrypts", share: krb5p, sess: encrypting(kerberos())},
		{name: "krb5p admits an encrypt_data tree the session can meet", share: krb5p, sess: canEncrypt(kerberos()), encryptTree: true},
		{name: "krb5p refuses an encrypt_data tree the session cannot meet", share: krb5p, sess: signing(kerberos()), encryptTree: true, wantDeny: true},
	}

	h := NewHandler()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.share.Name = "/share"
			err := h.checkSharePolicy(context.Background(), &tc.share, tc.sess, tc.sess.AuthzIdentity(), tc.encryptTree, "127.0.0.1:445")
			if tc.wantDeny {
				if !errors.Is(err, runtime.ErrShareAccessDenied) {
					t.Fatalf("err = %v, want a denial", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}
}

// TestReauthenticationRemovesTreeThePolicyRefuses pins the re-check a
// re-authentication runs: MS-SMB2 keeps the session's trees across it, so a
// Kerberos session that re-authenticates with NTLM would otherwise keep its
// tree on a Kerberos-only share.
func TestReauthenticationRemovesTreeThePolicyRefuses(t *testing.T) {
	rt, blockStoreID := newTestShareRuntime(t)
	if err := rt.RegisterMetadataStore("test-meta", badgertest.NewInMemory(t)); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:              "/export",
		MetadataStore:     "test-meta",
		BlockStoreID:      blockStoreID,
		Enabled:           true,
		DefaultPermission: "read-write",
		RequireKerberos:   true,
		RootAttr:          &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	h := NewHandler()
	h.Registry = rt

	user := &models.User{Username: "alice", Enabled: true}
	sess := h.CreateSessionWithUserAndExpiry(1, "127.0.0.1:1", user, "", time.Now().Add(time.Hour))
	treeID := h.GenerateTreeID()
	h.StoreTree(&TreeConnection{TreeID: treeID, SessionID: sess.SessionID, ShareName: "/export", Permission: models.PermissionReadWrite})

	if res := h.tryReauthUpdate(context.Background(), &PendingAuth{SessionID: sess.SessionID, IsReauth: true}, "alice", "", user, false); res == nil {
		t.Fatal("tryReauthUpdate returned nil for an existing session")
	}

	if _, ok := h.GetTree(treeID); ok {
		t.Error("the tree survived an NTLM re-authentication on a Kerberos-only share")
	}
}
