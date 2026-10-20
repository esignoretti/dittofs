package nbd

// Payload-length enforcement.
//
// The handshake advertises maxBlockSize as the maximum payload length
// (NBD_INFO_BLOCK_SIZE). Until this file existed nothing enforced it: the only
// bound on a request was the export size, so a client could ask for a READ of
// the whole export and the server would allocate exactly that.
//
// That was a real defect, not a theoretical one. Measured against the committed
// code with a 1 GiB export: a 256 MiB READ — 8x the advertised maximum — was
// accepted with reply error 0 and a 256 MiB allocation. The spec names this
// exact case as the one where NBD_EOVERFLOW is the correct answer:
//
//	"The server SHOULD NOT return NBD_EOVERFLOW except as documented in
//	 response to NBD_CMD_READ when NBD_CMD_FLAG_DF is supported, or when a
//	 command without payload requests a length larger than an advertised
//	 maximum payload length."
//
// READ is a command without payload, so the second clause applies directly.
//
// Two reasons this matters beyond conformance:
//
//  1. Memory. A single connection could make the server allocate up to the
//     export size in one request. With request pipelining (several commands in
//     flight) that becomes several exports' worth, and no in-flight byte budget
//     can bound memory when a single request is unbounded.
//  2. Symmetry. WRITE already had to be handled carefully because its payload
//     is in flight and must be drained; a length the server refuses should be
//     refused for the same reason on both commands rather than by accident of
//     which one allocates.

// payloadTooLarge reports whether length exceeds the maximum the handshake
// advertised. It is a function rather than an inline comparison so READ and
// WRITE cannot drift apart, and so the value is greppable from the handshake
// constant.
func payloadTooLarge(length uint32) bool {
	return length > maxBlockSize
}
