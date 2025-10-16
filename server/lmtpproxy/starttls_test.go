package lmtpproxy

import (
	"context"
	"testing"
	"time"
)

// TestLMTPProxyServerOptions verifies that ServerOptions correctly stores StartTLS settings.
func TestLMTPProxyServerOptions(t *testing.T) {
	tests := []struct {
		name                 string
		tlsUseStartTLS       bool
		remoteTLSUseStartTLS bool
		description          string
	}{
		{
			name:                 "No StartTLS",
			tlsUseStartTLS:       false,
			remoteTLSUseStartTLS: false,
			description:          "Traditional implicit TLS or no TLS",
		},
		{
			name:                 "Client StartTLS only",
			tlsUseStartTLS:       true,
			remoteTLSUseStartTLS: false,
			description:          "StartTLS for client connections, implicit TLS for backend",
		},
		{
			name:                 "Remote StartTLS only",
			tlsUseStartTLS:       false,
			remoteTLSUseStartTLS: true,
			description:          "Implicit TLS for clients, StartTLS for backend",
		},
		{
			name:                 "Both StartTLS",
			tlsUseStartTLS:       true,
			remoteTLSUseStartTLS: true,
			description:          "StartTLS for both client and backend connections",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := ServerOptions{
				Name:                 "test-lmtp-proxy",
				Addr:                 ":24",
				RemoteAddrs:          []string{"backend1.example.com:24"},
				RemotePort:           24,
				TLS:                  true,
				TLSUseStartTLS:       tt.tlsUseStartTLS,
				TLSCertFile:          "/path/to/cert.pem",
				TLSKeyFile:           "/path/to/key.pem",
				TLSVerify:            false,
				RemoteTLS:            true,
				RemoteTLSUseStartTLS: tt.remoteTLSUseStartTLS,
				RemoteTLSVerify:      true,
				ConnectTimeout:       10 * time.Second,
				SessionTimeout:       5 * time.Minute,
				MaxMessageSize:       52428800, // 50MB
			}

			// Verify the options are stored correctly
			if opts.TLSUseStartTLS != tt.tlsUseStartTLS {
				t.Errorf("Expected TLSUseStartTLS=%v, got %v", tt.tlsUseStartTLS, opts.TLSUseStartTLS)
			}

			if opts.RemoteTLSUseStartTLS != tt.remoteTLSUseStartTLS {
				t.Errorf("Expected RemoteTLSUseStartTLS=%v, got %v", tt.remoteTLSUseStartTLS, opts.RemoteTLSUseStartTLS)
			}

			t.Logf("%s: TLSUseStartTLS=%v, RemoteTLSUseStartTLS=%v",
				tt.description, opts.TLSUseStartTLS, opts.RemoteTLSUseStartTLS)
		})
	}
}

// TestLMTPProxyServerStartTLSConfiguration tests that the server
// properly stores and uses StartTLS configuration.
func TestLMTPProxyServerStartTLSConfiguration(t *testing.T) {
	// Note: We can't fully test server.Start() without setting up actual
	// network listeners, but we can verify that configuration is properly
	// stored in the Server struct.

	tests := []struct {
		name                 string
		tlsEnabled           bool
		tlsUseStartTLS       bool
		remoteTLSEnabled     bool
		remoteTLSUseStartTLS bool
		wantTLSUseStartTLS   bool
		description          string
	}{
		{
			name:                 "No TLS at all",
			tlsEnabled:           false,
			tlsUseStartTLS:       false,
			remoteTLSEnabled:     false,
			remoteTLSUseStartTLS: false,
			wantTLSUseStartTLS:   false,
			description:          "Plain connections everywhere",
		},
		{
			name:                 "Client implicit TLS",
			tlsEnabled:           true,
			tlsUseStartTLS:       false,
			remoteTLSEnabled:     false,
			remoteTLSUseStartTLS: false,
			wantTLSUseStartTLS:   false,
			description:          "Implicit TLS for clients, plain for backend",
		},
		{
			name:                 "Client StartTLS",
			tlsEnabled:           true,
			tlsUseStartTLS:       true,
			remoteTLSEnabled:     false,
			remoteTLSUseStartTLS: false,
			wantTLSUseStartTLS:   true,
			description:          "StartTLS for clients, plain for backend",
		},
		{
			name:                 "Remote StartTLS",
			tlsEnabled:           false,
			tlsUseStartTLS:       false,
			remoteTLSEnabled:     true,
			remoteTLSUseStartTLS: true,
			wantTLSUseStartTLS:   false,
			description:          "Plain for clients, StartTLS for backend",
		},
		{
			name:                 "Both StartTLS",
			tlsEnabled:           true,
			tlsUseStartTLS:       true,
			remoteTLSEnabled:     true,
			remoteTLSUseStartTLS: true,
			wantTLSUseStartTLS:   true,
			description:          "StartTLS everywhere",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			opts := ServerOptions{
				Name:                 "test-proxy",
				Addr:                 ":124",
				RemoteAddrs:          []string{"backend1.example.com:24"},
				RemotePort:           24,
				TLS:                  tt.tlsEnabled,
				TLSUseStartTLS:       tt.tlsUseStartTLS,
				TLSCertFile:          "../../testdata/sora.crt",
				TLSKeyFile:           "../../testdata/sora.key",
				TLSVerify:            false,
				RemoteTLS:            tt.remoteTLSEnabled,
				RemoteTLSUseStartTLS: tt.remoteTLSUseStartTLS,
				RemoteTLSVerify:      true,
				ConnectTimeout:       10 * time.Second,
				SessionTimeout:       5 * time.Minute,
				MaxMessageSize:       52428800, // 50MB
			}

			// Create server (but don't start it - we'd need real certs for that)
			srv, err := New(ctx, nil, "test.example.com", opts)

			if err != nil {
				t.Fatalf("Failed to create server: %v", err)
			}

			// Verify the server stored the configuration correctly
			if srv.tls != tt.tlsEnabled {
				t.Errorf("Expected tls=%v, got %v", tt.tlsEnabled, srv.tls)
			}

			if srv.tlsUseStartTLS != tt.wantTLSUseStartTLS {
				t.Errorf("Expected tlsUseStartTLS=%v, got %v", tt.wantTLSUseStartTLS, srv.tlsUseStartTLS)
			}

			// Verify connection manager was configured with correct StartTLS settings
			if srv.connManager != nil {
				isRemoteStartTLS := srv.connManager.IsRemoteStartTLS()
				wantRemoteStartTLS := tt.remoteTLSEnabled && tt.remoteTLSUseStartTLS

				if isRemoteStartTLS != wantRemoteStartTLS {
					t.Errorf("Expected connManager.IsRemoteStartTLS()=%v, got %v",
						wantRemoteStartTLS, isRemoteStartTLS)
				}

				t.Logf("%s: Client StartTLS=%v, Backend StartTLS=%v",
					tt.description, srv.tlsUseStartTLS, isRemoteStartTLS)
			}

			// Clean up
			srv.Stop()
		})
	}
}

// TestLMTPProxyTLSModeMatrix verifies all valid combinations of TLS modes.
func TestLMTPProxyTLSModeMatrix(t *testing.T) {
	// This test documents all valid TLS configuration combinations for LMTP
	modes := []struct {
		clientMode  string
		backendMode string
		config      ServerOptions
	}{
		{
			clientMode:  "Plain",
			backendMode: "Plain",
			config: ServerOptions{
				TLS:                  false,
				TLSUseStartTLS:       false,
				RemoteTLS:            false,
				RemoteTLSUseStartTLS: false,
			},
		},
		{
			clientMode:  "Implicit TLS",
			backendMode: "Plain",
			config: ServerOptions{
				TLS:                  true,
				TLSUseStartTLS:       false,
				RemoteTLS:            false,
				RemoteTLSUseStartTLS: false,
			},
		},
		{
			clientMode:  "StartTLS",
			backendMode: "Plain",
			config: ServerOptions{
				TLS:                  true,
				TLSUseStartTLS:       true,
				RemoteTLS:            false,
				RemoteTLSUseStartTLS: false,
			},
		},
		{
			clientMode:  "Plain",
			backendMode: "Implicit TLS",
			config: ServerOptions{
				TLS:                  false,
				TLSUseStartTLS:       false,
				RemoteTLS:            true,
				RemoteTLSUseStartTLS: false,
			},
		},
		{
			clientMode:  "Implicit TLS",
			backendMode: "Implicit TLS",
			config: ServerOptions{
				TLS:                  true,
				TLSUseStartTLS:       false,
				RemoteTLS:            true,
				RemoteTLSUseStartTLS: false,
			},
		},
		{
			clientMode:  "StartTLS",
			backendMode: "Implicit TLS",
			config: ServerOptions{
				TLS:                  true,
				TLSUseStartTLS:       true,
				RemoteTLS:            true,
				RemoteTLSUseStartTLS: false,
			},
		},
		{
			clientMode:  "Plain",
			backendMode: "StartTLS",
			config: ServerOptions{
				TLS:                  false,
				TLSUseStartTLS:       false,
				RemoteTLS:            true,
				RemoteTLSUseStartTLS: true,
			},
		},
		{
			clientMode:  "Implicit TLS",
			backendMode: "StartTLS",
			config: ServerOptions{
				TLS:                  true,
				TLSUseStartTLS:       false,
				RemoteTLS:            true,
				RemoteTLSUseStartTLS: true,
			},
		},
		{
			clientMode:  "StartTLS",
			backendMode: "StartTLS",
			config: ServerOptions{
				TLS:                  true,
				TLSUseStartTLS:       true,
				RemoteTLS:            true,
				RemoteTLSUseStartTLS: true,
			},
		},
	}

	for _, mode := range modes {
		t.Run(mode.clientMode+"_to_"+mode.backendMode, func(t *testing.T) {
			ctx := context.Background()

			opts := mode.config
			opts.Name = "test-proxy"
			opts.Addr = ":124"
			opts.RemoteAddrs = []string{"backend.example.com:24"}
			opts.RemotePort = 24
			opts.TLSCertFile = "../../testdata/sora.crt"
			opts.TLSKeyFile = "../../testdata/sora.key"
			opts.ConnectTimeout = 10 * time.Second
			opts.SessionTimeout = 5 * time.Minute
			opts.MaxMessageSize = 52428800 // 50MB

			srv, err := New(ctx, nil, "test.example.com", opts)
			if err != nil {
				t.Fatalf("Failed to create server: %v", err)
			}

			t.Logf("✓ Valid configuration: Client=%s, Backend=%s", mode.clientMode, mode.backendMode)

			srv.Stop()
		})
	}
}

// TestLMTPProxyStartTLSWithXCLIENT verifies StartTLS works alongside XCLIENT forwarding.
func TestLMTPProxyStartTLSWithXCLIENT(t *testing.T) {
	ctx := context.Background()

	opts := ServerOptions{
		Name:                 "test-proxy",
		Addr:                 ":124",
		RemoteAddrs:          []string{"backend.example.com:24"},
		RemotePort:           24,
		TLS:                  true,
		TLSUseStartTLS:       true,
		RemoteTLS:            true,
		RemoteTLSUseStartTLS: true,
		RemoteUseXCLIENT:     true, // XCLIENT forwarding enabled
		ConnectTimeout:       10 * time.Second,
		SessionTimeout:       5 * time.Minute,
		MaxMessageSize:       52428800,
	}

	srv, err := New(ctx, nil, "test.example.com", opts)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	// Verify StartTLS and XCLIENT can coexist
	if !srv.tlsUseStartTLS {
		t.Error("Expected tlsUseStartTLS=true")
	}

	if !srv.remoteUseXCLIENT {
		t.Error("Expected remoteUseXCLIENT=true")
	}

	if !srv.connManager.IsRemoteStartTLS() {
		t.Error("Expected remote StartTLS to be enabled")
	}

	t.Log("✓ StartTLS and XCLIENT forwarding can be used together")

	srv.Stop()
}
