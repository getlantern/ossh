package ossh

// The sshReadWriteClosers returned by the handshake functions below use an
// obfuscator.ObfuscatedSSHConn as the underlying transport. This connection does not define
// behavior for concurrent calls to one of Read or Write:
//
// https://pkg.go.dev/github.com/Psiphon-Labs/psiphon-tunnel-core@v2.0.14+incompatible/psiphon/common/obfuscator#ObfuscatedSSHConn
//
// It so happens that we always wrap sshReadWriteClosers in a deadlineReadWriter, which enforces
// single-threaded Reads and Writes anyway. If this changes or if the behavior of deadlineReadWriter
// changes, we should ensure that we still enforce single-threaded Reads and Writes to prevent
// subtle errors.
// TODO: think about a better way to structure the code in light of the above

// TODO: think about multiplexing via channels rather than layering multiplexing on top
