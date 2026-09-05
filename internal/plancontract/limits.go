// Package plancontract defines the byte-size contract shared by plan
// execution, transport, storage, and reconstruction.
package plancontract

const (
	// MaxExecutableBytes is the maximum complete native plan document. Every
	// byte is counted, including a trailing newline.
	MaxExecutableBytes int64 = 8 << 20

	// ChunkBytes keeps a ConfigMap BinaryData value below 1 MiB after the
	// Kubernetes JSON transport base64-encodes it, with room for object
	// metadata. MaxChunks is derived so storage cannot advertise more capacity
	// than the runner can execute.
	ChunkBytes = 512 << 10
	MaxChunks  = int((MaxExecutableBytes + ChunkBytes - 1) / ChunkBytes)

	// encoding/json can expand one source byte to a six-byte escape (for
	// example, '<' becomes "\u003c"). The fixed headroom covers every bounded
	// non-plan field in a successful runner result and keeps the largest valid
	// payload strictly below the parser cap.
	maxJSONByteExpansion        = 6
	resultEnvelopeBytes         = 64 << 10
	MaxResultPayloadBytes int64 = MaxExecutableBytes*maxJSONByteExpansion + resultEnvelopeBytes
)
