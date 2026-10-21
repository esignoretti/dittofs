package nbd

// ConfigForTest exposes the resolved config to the external test package.
//
// A resolved UID cannot be observed through New: unlike an out-of-range port or
// an uncapped remote bind, `uid: 0` is a perfectly valid config that quietly
// resolves to the wrong identity, so nothing panics and there is nothing to
// assert on. Adapter.config is unexported and Serve builds the identity from it
// (adapter.go:118) without exposing it, so the alternative would be widening the
// production API with a UID accessor that no caller needs.
//
// This file is `package nbd` while its siblings are `package nbd_test`, which Go
// allows: only a non-test file constrains the package of the directory's test
// files, and a `_test.go` file may declare either. nbd_test sees ConfigForTest
// (an exported method on an exported type); ConfigForTest sees a.config.
func (a *Adapter) ConfigForTest() Config { return a.config }
