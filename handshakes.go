package ossh

import (
	"errors"
	"fmt"
	"net"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/obfuscator"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/prng"
	"golang.org/x/crypto/ssh"
)

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

func discardChannels(chans <-chan ssh.NewChannel) {
	for newChan := range chans {
		newChan.Reject(ssh.ResourceShortage, "not accepting any more channels")
	}
}

func clientHandshake(transport net.Conn, cfg DialerConfig) func() (ssh.Conn, ssh.Channel, error) {
	return func() (ssh.Conn, ssh.Channel, error) {
		if cfg.ServerPublicKey == nil {
			return nil, nil, errors.New("server public key must be configured")
		}
		if cfg.ObfuscationKeyword == "" {
			return nil, nil, errors.New("obfuscation keywork must be configured")
		}

		prngSeed, err := prng.NewSeed()
		if err != nil {
			return nil, nil, fmt.Errorf("failed to generate PRNG seed: %w", err)
		}

		osshConn, err := obfuscator.NewClientObfuscatedSSHConn(
			transport,
			cfg.ObfuscationKeyword,
			prngSeed,
			// Set min/max padding to nil to use obfuscator package defaults.
			nil, nil,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("ossh handshake failed: %w", err)
		}

		sshCfg := ssh.ClientConfig{HostKeyCallback: ssh.FixedHostKey(cfg.ServerPublicKey)}
		sshConn, chans, reqs, err := ssh.NewClientConn(osshConn, "", &sshCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("ssh handshake failed: %w", err)
		}
		go discardChannels(chans)
		go ssh.DiscardRequests(reqs)

		channel, reqs, err := sshConn.OpenChannel("channel0", []byte{})
		if err != nil {
			return nil, nil, fmt.Errorf("failed to open channel: %w", err)
		}
		go ssh.DiscardRequests(reqs)

		return sshConn, channel, nil
	}
}

func serverHandshake(transport net.Conn, cfg ListenerConfig) func() (ssh.Conn, ssh.Channel, error) {
	return func() (ssh.Conn, ssh.Channel, error) {
		if cfg.HostKey == nil {
			return nil, nil, errors.New("host key must be configured")
		}
		if cfg.ObfuscationKeyword == "" {
			return nil, nil, errors.New("obfuscation keywork must be configured")
		}

		osshConn, err := obfuscator.NewServerObfuscatedSSHConn(
			transport,
			cfg.ObfuscationKeyword,
			obfuscator.NewSeedHistory(nil), // use the obfuscator package defaults
			cfg.logger(),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("ossh handshake failed: %w", err)
		}

		sshCfg := ssh.ServerConfig{NoClientAuth: true}
		sshCfg.AddHostKey(cfg.HostKey)

		sshConn, chans, reqs, err := ssh.NewServerConn(osshConn, &sshCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("ssh handshake failed: %w", err)
		}
		go ssh.DiscardRequests(reqs)

		channel, reqs, err := (<-chans).Accept()
		if err != nil {
			return nil, nil, fmt.Errorf("failed to accept channel: %w", err)
		}
		go discardChannels(chans)
		go ssh.DiscardRequests(reqs)

		return sshConn, channel, nil
	}
}

func newClientConn(transport net.Conn, cfg DialerConfig) *fullConn {
	return newFullConn(newOSSHConn(clientHandshake(transport, cfg)))
}

func newServerConn(transport net.Conn, cfg ListenerConfig) *fullConn {
	return newFullConn(newOSSHConn(serverHandshake(transport, cfg)))
}
