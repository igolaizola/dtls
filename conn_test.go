package dtls

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/dtls/v2/internal/net/dpipe"
	"github.com/pion/dtls/v2/pkg/crypto/selfsign"
	"github.com/pion/transport/test"
)

func TestStressDuplex(t *testing.T) {
	// Limit runtime in case of deadlocks
	lim := test.TimeOut(time.Second * 20)
	defer lim.Stop()

	// Check for leaking routines
	report := test.CheckRoutines(t)
	defer report()

	// Run the test
	stressDuplex(t)
}

func stressDuplex(t *testing.T) {
	ca, cb, err := pipeMemory()
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		err = ca.Close()
		if err != nil {
			t.Fatal(err)
		}
		err = cb.Close()
		if err != nil {
			t.Fatal(err)
		}
	}()

	opt := test.Options{
		MsgSize:  2048,
		MsgCount: 100,
	}

	err = test.StressDuplex(ca, cb, opt)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRoutineLeakOnClose(t *testing.T) {
	// Limit runtime in case of deadlocks
	lim := test.TimeOut(5 * time.Second)
	defer lim.Stop()

	// Check for leaking routines
	report := test.CheckRoutines(t)
	defer report()

	ca, cb, err := pipeMemory()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ca.Write(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	if err := cb.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ca.Close(); err != nil {
		t.Fatal(err)
	}
	// Packet is sent, but not read.
	// inboundLoop routine should not be leaked.
}

func TestReadWriteDeadline(t *testing.T) {
	// Limit runtime in case of deadlocks
	lim := test.TimeOut(5 * time.Second)
	defer lim.Stop()

	// Check for leaking routines
	report := test.CheckRoutines(t)
	defer report()

	ca, cb, err := pipeMemory()
	if err != nil {
		t.Fatal(err)
	}

	if err := ca.SetDeadline(time.Unix(0, 1)); err != nil {
		t.Fatal(err)
	}
	_, werr := ca.Write(make([]byte, 100))
	if e, ok := werr.(net.Error); ok {
		if !e.Timeout() {
			t.Error("Deadline exceeded Write must return Timeout error")
		}
		if !e.Temporary() {
			t.Error("Deadline exceeded Write must return Temporary error")
		}
	} else {
		t.Error("Write must return net.Error error")
	}
	_, rerr := ca.Read(make([]byte, 100))
	if e, ok := rerr.(net.Error); ok {
		if !e.Timeout() {
			t.Error("Deadline exceeded Read must return Timeout error")
		}
		if !e.Temporary() {
			t.Error("Deadline exceeded Read must return Temporary error")
		}
	} else {
		t.Error("Read must return net.Error error")
	}
	if err := ca.SetDeadline(time.Time{}); err != nil {
		t.Error(err)
	}

	if err := ca.Close(); err != nil {
		t.Error(err)
	}
	if err := cb.Close(); err != nil {
		t.Error(err)
	}

	if _, err := ca.Write(make([]byte, 100)); err != ErrConnClosed {
		t.Errorf("Write must return %v after close, got %v", ErrConnClosed, err)
	}
	if _, err := ca.Read(make([]byte, 100)); err != io.EOF {
		t.Errorf("Read must return %v after close, got %v", io.EOF, err)
	}
}

func TestSequenceNumberOverflow(t *testing.T) {
	// Limit runtime in case of deadlocks
	lim := test.TimeOut(5 * time.Second)
	defer lim.Stop()

	// Check for leaking routines
	report := test.CheckRoutines(t)
	defer report()

	t.Run("ApplicationData", func(t *testing.T) {
		ca, cb, err := pipeMemory()
		if err != nil {
			t.Fatal(err)
		}

		atomic.StoreUint64(&ca.state.localSequenceNumber[1], maxSequenceNumber)
		if _, werr := ca.Write(make([]byte, 100)); werr != nil {
			t.Errorf("Write must send message with maximum sequence number, but errord: %v", werr)
		}
		if _, werr := ca.Write(make([]byte, 100)); werr != errSequenceNumberOverflow {
			t.Errorf("Write must abandonsend message with maximum sequence number, but errord: %v", werr)
		}

		if err := ca.Close(); err != nil {
			t.Error(err)
		}
		if err := cb.Close(); err != nil {
			t.Error(err)
		}
	})
	t.Run("Handshake", func(t *testing.T) {
		ca, cb, err := pipeMemory()
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		atomic.StoreUint64(&ca.state.localSequenceNumber[0], maxSequenceNumber+1)

		// Try to send handshake packet.
		if werr := ca.writePackets(ctx, []*packet{
			{
				record: &recordLayer{
					recordLayerHeader: recordLayerHeader{
						protocolVersion: protocolVersion1_2,
					},
					content: &handshake{
						handshakeMessage: &handshakeMessageClientHello{
							version:            protocolVersion1_2,
							cookie:             make([]byte, 64),
							cipherSuites:       defaultCipherSuites(),
							compressionMethods: defaultCompressionMethods,
						}},
				},
			},
		}); werr != errSequenceNumberOverflow {
			t.Errorf("Connection must fail on handshake packet reaches maximum sequence number")
		}

		if err := ca.Close(); err != nil {
			t.Error(err)
		}
		if err := cb.Close(); err != nil {
			t.Error(err)
		}
	})
}

func pipeMemory() (*Conn, *Conn, error) {
	// In memory pipe
	ca, cb := dpipe.Pipe()
	return pipeConn(ca, cb)
}

func pipeConn(ca, cb net.Conn) (*Conn, *Conn, error) {
	type result struct {
		c   *Conn
		err error
	}

	c := make(chan result)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Setup client
	go func() {
		client, err := testClient(ctx, ca, &Config{SRTPProtectionProfiles: []SRTPProtectionProfile{SRTP_AES128_CM_HMAC_SHA1_80}}, true)
		c <- result{client, err}
	}()

	// Setup server
	server, err := testServer(ctx, cb, &Config{SRTPProtectionProfiles: []SRTPProtectionProfile{SRTP_AES128_CM_HMAC_SHA1_80}}, true)
	if err != nil {
		return nil, nil, err
	}

	// Receive client
	res := <-c
	if res.err != nil {
		return nil, nil, res.err
	}

	return res.c, server, nil
}

func testClient(ctx context.Context, c net.Conn, cfg *Config, generateCertificate bool) (*Conn, error) {
	if generateCertificate {
		clientCert, err := selfsign.GenerateSelfSigned()
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{clientCert}
	}
	cfg.InsecureSkipVerify = true
	return ClientWithContext(ctx, c, cfg)
}

func testServer(ctx context.Context, c net.Conn, cfg *Config, generateCertificate bool) (*Conn, error) {
	if generateCertificate {
		serverCert, err := selfsign.GenerateSelfSigned()
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{serverCert}
	}
	return ServerWithContext(ctx, c, cfg)
}

func TestHandshakeWithAlert(t *testing.T) {
	// Limit runtime in case of deadlocks
	lim := test.TimeOut(time.Second * 20)
	defer lim.Stop()

	// Check for leaking routines
	report := test.CheckRoutines(t)
	defer report()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cases := map[string]struct {
		configServer, configClient *Config
		errServer, errClient       error
	}{
		"CipherSuiteNoIntersection": {
			configServer: &Config{
				CipherSuites: []CipherSuiteID{TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
			},
			configClient: &Config{
				CipherSuites: []CipherSuiteID{TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
			},
			errServer: errCipherSuiteNoIntersection,
			errClient: &errAlert{&alert{alertLevelFatal, alertInsufficientSecurity}},
		},
		"SignatureSchemesNoIntersection": {
			configServer: &Config{
				CipherSuites:     []CipherSuiteID{TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
				SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256},
			},
			configClient: &Config{
				CipherSuites:     []CipherSuiteID{TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
				SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP521AndSHA512},
			},
			errServer: &errAlert{&alert{alertLevelFatal, alertInsufficientSecurity}},
			errClient: errNoAvailableSignatureSchemes,
		},
	}

	for name, testCase := range cases {
		testCase := testCase
		t.Run(name, func(t *testing.T) {
			clientErr := make(chan error, 1)

			ca, cb := dpipe.Pipe()
			go func() {
				_, err := testClient(ctx, ca, testCase.configClient, true)
				clientErr <- err
			}()

			_, errServer := testServer(ctx, cb, testCase.configServer, true)
			if !errors.Is(errServer, testCase.errServer) {
				t.Fatalf("Server error exp(%v) failed(%v)", testCase.errServer, errServer)
			}

			errClient := <-clientErr
			if !errors.Is(errClient, testCase.errClient) {
				t.Fatalf("Client error exp(%v) failed(%v)", testCase.errClient, errClient)
			}
		})
	}
}

func TestExportKeyingMaterial(t *testing.T) {
	// Check for leaking routines
	report := test.CheckRoutines(t)
	defer report()

	var rand [28]byte
	exportLabel := "EXTRACTOR-dtls_srtp"

	expectedServerKey := []byte{0x61, 0x09, 0x9d, 0x7d, 0xcb, 0x08, 0x52, 0x2c, 0xe7, 0x7b}
	expectedClientKey := []byte{0x87, 0xf0, 0x40, 0x02, 0xf6, 0x1c, 0xf1, 0xfe, 0x8c, 0x77}

	c := &Conn{
		state: State{
			localRandom:         handshakeRandom{time.Unix(500, 0), rand},
			remoteRandom:        handshakeRandom{time.Unix(1000, 0), rand},
			localSequenceNumber: []uint64{0, 0},
			cipherSuite:         &cipherSuiteTLSEcdheEcdsaWithAes128GcmSha256{},
		},
	}
	c.setLocalEpoch(0)
	c.setRemoteEpoch(0)

	state := c.ConnectionState()
	_, err := state.ExportKeyingMaterial(exportLabel, nil, 0)
	if err != errHandshakeInProgress {
		t.Errorf("ExportKeyingMaterial when epoch == 0: expected '%s' actual '%s'", errHandshakeInProgress, err)
	}

	c.setLocalEpoch(1)
	state = c.ConnectionState()
	_, err = state.ExportKeyingMaterial(exportLabel, []byte{0x00}, 0)
	if err != errContextUnsupported {
		t.Errorf("ExportKeyingMaterial with context: expected '%s' actual '%s'", errContextUnsupported, err)
	}

	for k := range invalidKeyingLabels {
		state = c.ConnectionState()
		_, err = state.ExportKeyingMaterial(k, nil, 0)
		if err != errReservedExportKeyingMaterial {
			t.Errorf("ExportKeyingMaterial reserved label: expected '%s' actual '%s'", errReservedExportKeyingMaterial, err)
		}
	}

	state = c.ConnectionState()
	keyingMaterial, err := state.ExportKeyingMaterial(exportLabel, nil, 10)
	if err != nil {
		t.Errorf("ExportKeyingMaterial as server: unexpected error '%s'", err)
	} else if !bytes.Equal(keyingMaterial, expectedServerKey) {
		t.Errorf("ExportKeyingMaterial client export: expected (% 02x) actual (% 02x)", expectedServerKey, keyingMaterial)
	}

	c.state.isClient = true
	state = c.ConnectionState()
	keyingMaterial, err = state.ExportKeyingMaterial(exportLabel, nil, 10)
	if err != nil {
		t.Errorf("ExportKeyingMaterial as server: unexpected error '%s'", err)
	} else if !bytes.Equal(keyingMaterial, expectedClientKey) {
		t.Errorf("ExportKeyingMaterial client export: expected (% 02x) actual (% 02x)", expectedClientKey, keyingMaterial)
	}
}

func TestPSK(t *testing.T) {
	// Limit runtime in case of deadlocks
	lim := test.TimeOut(time.Second * 20)
	defer lim.Stop()

	// Check for leaking routines
	report := test.CheckRoutines(t)
	defer report()

	for _, test := range []struct {
		Name           string
		ServerIdentity []byte
	}{
		{
			Name:           "Server identity specified",
			ServerIdentity: []byte("Test Identity"),
		},
		{
			Name:           "Server identity nil",
			ServerIdentity: nil,
		},
	} {
		test := test
		t.Run(test.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			clientIdentity := []byte("Client Identity")
			type result struct {
				c   *Conn
				err error
			}
			clientRes := make(chan result, 1)

			ca, cb := dpipe.Pipe()
			go func() {
				conf := &Config{
					PSK: func(hint []byte) ([]byte, error) {
						if !bytes.Equal(test.ServerIdentity, hint) { // nolint
							return nil, fmt.Errorf("TestPSK: Client got invalid identity expected(% 02x) actual(% 02x)", test.ServerIdentity, hint) // nolint
						}

						return []byte{0xAB, 0xC1, 0x23}, nil
					},
					PSKIdentityHint: clientIdentity,
					CipherSuites:    []CipherSuiteID{TLS_PSK_WITH_AES_128_CCM_8},
				}

				c, err := testClient(ctx, ca, conf, false)
				clientRes <- result{c, err}
			}()

			config := &Config{
				PSK: func(hint []byte) ([]byte, error) {
					if !bytes.Equal(clientIdentity, hint) {
						return nil, fmt.Errorf("TestPSK: Server got invalid identity expected(% 02x) actual(% 02x)", clientIdentity, hint)
					}
					return []byte{0xAB, 0xC1, 0x23}, nil
				},
				PSKIdentityHint: test.ServerIdentity,
				CipherSuites:    []CipherSuiteID{TLS_PSK_WITH_AES_128_CCM_8},
			}

			server, err := testServer(ctx, cb, config, false)
			if err != nil {
				t.Fatalf("TestPSK: Server failed(%v)", err)
			}
			defer func() {
				_ = server.Close()
			}()

			res := <-clientRes
			if res.err != nil {
				t.Fatal(res.err)
			}
			_ = res.c.Close()
		})
	}
}

func TestPSKHintFail(t *testing.T) {
	// Check for leaking routines
	report := test.CheckRoutines(t)
	defer report()

	serverAlertError := &errAlert{&alert{alertLevelFatal, alertInternalError}}
	pskRejected := errors.New("PSK Rejected")

	// Limit runtime in case of deadlocks
	lim := test.TimeOut(time.Second * 20)
	defer lim.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	clientErr := make(chan error, 1)

	ca, cb := dpipe.Pipe()
	go func() {
		conf := &Config{
			PSK: func(hint []byte) ([]byte, error) {
				return nil, pskRejected
			},
			PSKIdentityHint: []byte{},
			CipherSuites:    []CipherSuiteID{TLS_PSK_WITH_AES_128_CCM_8},
		}

		_, err := testClient(ctx, ca, conf, false)
		clientErr <- err
	}()

	config := &Config{
		PSK: func(hint []byte) ([]byte, error) {
			return nil, pskRejected
		},
		PSKIdentityHint: []byte{},
		CipherSuites:    []CipherSuiteID{TLS_PSK_WITH_AES_128_CCM_8},
	}

	if _, err := testServer(ctx, cb, config, false); !errors.Is(err, serverAlertError) {
		t.Fatalf("TestPSK: Server error exp(%v) failed(%v)", serverAlertError, err)
	}

	if err := <-clientErr; !errors.Is(err, pskRejected) {
		t.Fatalf("TestPSK: Client error exp(%v) failed(%v)", pskRejected, err)
	}
}

func TestClientTimeout(t *testing.T) {
	// Limit runtime in case of deadlocks
	lim := test.TimeOut(time.Second * 20)
	defer lim.Stop()

	// Check for leaking routines
	report := test.CheckRoutines(t)
	defer report()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	clientErr := make(chan error, 1)

	ca, _ := dpipe.Pipe()
	go func() {
		conf := &Config{}

		c, err := testClient(ctx, ca, conf, true)
		if err == nil {
			_ = c.Close()
		}
		clientErr <- err
	}()

	// no server!
	err := <-clientErr
	if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("Client error exp(Temporary network error) failed(%v)", err)
	}
}

func TestSRTPConfiguration(t *testing.T) {
	// Check for leaking routines
	report := test.CheckRoutines(t)
	defer report()

	for _, test := range []struct {
		Name            string
		ClientSRTP      []SRTPProtectionProfile
		ServerSRTP      []SRTPProtectionProfile
		ExpectedProfile SRTPProtectionProfile
		WantClientError error
		WantServerError error
	}{
		{
			Name:            "No SRTP in use",
			ClientSRTP:      nil,
			ServerSRTP:      nil,
			ExpectedProfile: 0,
			WantClientError: nil,
			WantServerError: nil,
		},
		{
			Name:            "SRTP both ends",
			ClientSRTP:      []SRTPProtectionProfile{SRTP_AES128_CM_HMAC_SHA1_80},
			ServerSRTP:      []SRTPProtectionProfile{SRTP_AES128_CM_HMAC_SHA1_80},
			ExpectedProfile: SRTP_AES128_CM_HMAC_SHA1_80,
			WantClientError: nil,
			WantServerError: nil,
		},
		{
			Name:            "SRTP client only",
			ClientSRTP:      []SRTPProtectionProfile{SRTP_AES128_CM_HMAC_SHA1_80},
			ServerSRTP:      nil,
			ExpectedProfile: 0,
			WantClientError: &errAlert{&alert{alertLevelFatal, alertInsufficientSecurity}},
			WantServerError: errServerNoMatchingSRTPProfile,
		},
		{
			Name:            "SRTP server only",
			ClientSRTP:      nil,
			ServerSRTP:      []SRTPProtectionProfile{SRTP_AES128_CM_HMAC_SHA1_80},
			ExpectedProfile: 0,
			WantClientError: nil,
			WantServerError: nil,
		},
		{
			Name:            "Multiple Suites",
			ClientSRTP:      []SRTPProtectionProfile{SRTP_AES128_CM_HMAC_SHA1_80, SRTP_AES128_CM_HMAC_SHA1_32},
			ServerSRTP:      []SRTPProtectionProfile{SRTP_AES128_CM_HMAC_SHA1_80, SRTP_AES128_CM_HMAC_SHA1_32},
			ExpectedProfile: SRTP_AES128_CM_HMAC_SHA1_80,
			WantClientError: nil,
			WantServerError: nil,
		},
		{
			Name:            "Multiple Suites, Client Chooses",
			ClientSRTP:      []SRTPProtectionProfile{SRTP_AES128_CM_HMAC_SHA1_80, SRTP_AES128_CM_HMAC_SHA1_32},
			ServerSRTP:      []SRTPProtectionProfile{SRTP_AES128_CM_HMAC_SHA1_32, SRTP_AES128_CM_HMAC_SHA1_80},
			ExpectedProfile: SRTP_AES128_CM_HMAC_SHA1_80,
			WantClientError: nil,
			WantServerError: nil,
		},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		ca, cb := dpipe.Pipe()
		type result struct {
			c   *Conn
			err error
		}
		c := make(chan result)

		go func() {
			client, err := testClient(ctx, ca, &Config{SRTPProtectionProfiles: test.ClientSRTP}, true)
			c <- result{client, err}
		}()

		server, err := testServer(ctx, cb, &Config{SRTPProtectionProfiles: test.ServerSRTP}, true)
		if !errors.Is(err, test.WantServerError) {
			t.Errorf("TestSRTPConfiguration: Server Error Mismatch '%s': expected(%v) actual(%v)", test.Name, test.WantServerError, err)
		}
		if err == nil {
			defer func() {
				_ = server.Close()
			}()
		}

		res := <-c
		if res.err == nil {
			defer func() {
				_ = res.c.Close()
			}()
		}
		if !errors.Is(res.err, test.WantClientError) {
			t.Fatalf("TestSRTPConfiguration: Client Error Mismatch '%s': expected(%v) actual(%v)", test.Name, test.WantClientError, res.err)
		}
		if res.c == nil {
			return
		}

		actualClientSRTP, _ := res.c.SelectedSRTPProtectionProfile()
		if actualClientSRTP != test.ExpectedProfile {
			t.Errorf("TestSRTPConfiguration: Client SRTPProtectionProfile Mismatch '%s': expected(%v) actual(%v)", test.Name, test.ExpectedProfile, actualClientSRTP)
		}

		actualServerSRTP, _ := server.SelectedSRTPProtectionProfile()
		if actualServerSRTP != test.ExpectedProfile {
			t.Errorf("TestSRTPConfiguration: Server SRTPProtectionProfile Mismatch '%s': expected(%v) actual(%v)", test.Name, test.ExpectedProfile, actualServerSRTP)
		}
	}
}

func TestClientCertificate(t *testing.T) {
	// Check for leaking routines
	report := test.CheckRoutines(t)
	defer report()

	srvCert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	srvCAPool := x509.NewCertPool()
	srvCertificate, err := x509.ParseCertificate(srvCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	srvCAPool.AddCert(srvCertificate)

	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	caPool := x509.NewCertPool()
	caPool.AddCert(certificate)

	t.Run("parallel", func(t *testing.T) { // sync routines to check routine leak
		tests := map[string]struct {
			clientCfg *Config
			serverCfg *Config
			wantErr   bool
		}{
			"NoClientCert": {
				clientCfg: &Config{RootCAs: srvCAPool},
				serverCfg: &Config{
					Certificates: []tls.Certificate{srvCert},
					ClientAuth:   NoClientCert,
					ClientCAs:    caPool,
				},
			},
			"NoClientCert_cert": {
				clientCfg: &Config{RootCAs: srvCAPool, Certificates: []tls.Certificate{cert}},
				serverCfg: &Config{
					Certificates: []tls.Certificate{srvCert},
					ClientAuth:   RequireAnyClientCert,
				},
			},
			"RequestClientCert_cert": {
				clientCfg: &Config{RootCAs: srvCAPool, Certificates: []tls.Certificate{cert}},
				serverCfg: &Config{
					Certificates: []tls.Certificate{srvCert},
					ClientAuth:   RequestClientCert,
				},
			},
			"RequestClientCert_no_cert": {
				clientCfg: &Config{RootCAs: srvCAPool},
				serverCfg: &Config{
					Certificates: []tls.Certificate{srvCert},
					ClientAuth:   RequestClientCert,
					ClientCAs:    caPool,
				},
			},
			"RequireAnyClientCert": {
				clientCfg: &Config{RootCAs: srvCAPool, Certificates: []tls.Certificate{cert}},
				serverCfg: &Config{
					Certificates: []tls.Certificate{srvCert},
					ClientAuth:   RequireAnyClientCert,
				},
			},
			"RequireAnyClientCert_error": {
				clientCfg: &Config{RootCAs: srvCAPool},
				serverCfg: &Config{
					Certificates: []tls.Certificate{srvCert},
					ClientAuth:   RequireAnyClientCert,
				},
				wantErr: true,
			},
			"VerifyClientCertIfGiven_no_cert": {
				clientCfg: &Config{RootCAs: srvCAPool},
				serverCfg: &Config{
					Certificates: []tls.Certificate{srvCert},
					ClientAuth:   VerifyClientCertIfGiven,
					ClientCAs:    caPool,
				},
			},
			"VerifyClientCertIfGiven_cert": {
				clientCfg: &Config{RootCAs: srvCAPool, Certificates: []tls.Certificate{cert}},
				serverCfg: &Config{
					Certificates: []tls.Certificate{srvCert},
					ClientAuth:   VerifyClientCertIfGiven,
					ClientCAs:    caPool,
				},
			},
			"VerifyClientCertIfGiven_error": {
				clientCfg: &Config{RootCAs: srvCAPool, Certificates: []tls.Certificate{cert}},
				serverCfg: &Config{
					Certificates: []tls.Certificate{srvCert},
					ClientAuth:   VerifyClientCertIfGiven,
				},
				wantErr: true,
			},
			"RequireAndVerifyClientCert": {
				clientCfg: &Config{RootCAs: srvCAPool, Certificates: []tls.Certificate{cert}},
				serverCfg: &Config{
					Certificates: []tls.Certificate{srvCert},
					ClientAuth:   RequireAndVerifyClientCert,
					ClientCAs:    caPool,
				},
			},
		}
		for name, tt := range tests {
			tt := tt
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				ca, cb := dpipe.Pipe()
				type result struct {
					c   *Conn
					err error
				}
				c := make(chan result)

				go func() {
					client, err := Client(ca, tt.clientCfg)
					c <- result{client, err}
				}()

				server, err := Server(cb, tt.serverCfg)
				res := <-c
				defer func() {
					if err == nil {
						_ = server.Close()
					}
					if res.err == nil {
						_ = res.c.Close()
					}
				}()

				if tt.wantErr {
					if err != nil {
						// Error expected, test succeeded
						return
					}
					t.Error("Error expected")
				}
				if err != nil {
					t.Errorf("Server failed(%v)", err)
				}

				if res.err != nil {
					t.Errorf("Client failed(%v)", res.err)
				}

				actualClientCert := server.ConnectionState().PeerCertificates
				if tt.serverCfg.ClientAuth == RequireAnyClientCert || tt.serverCfg.ClientAuth == RequireAndVerifyClientCert {
					if actualClientCert == nil {
						t.Errorf("Client did not provide a certificate")
					}

					if len(actualClientCert) != len(tt.clientCfg.Certificates[0].Certificate) || !bytes.Equal(tt.clientCfg.Certificates[0].Certificate[0], actualClientCert[0]) {
						t.Errorf("Client certificate was not communicated correctly")
					}
				}
				if tt.serverCfg.ClientAuth == NoClientCert {
					if actualClientCert != nil {
						t.Errorf("Client certificate wasn't expected")
					}
				}

				actualServerCert := res.c.ConnectionState().PeerCertificates
				if actualServerCert == nil {
					t.Errorf("Server did not provide a certificate")
				}

				if len(actualServerCert) != len(tt.serverCfg.Certificates[0].Certificate) || !bytes.Equal(tt.serverCfg.Certificates[0].Certificate[0], actualServerCert[0]) {
					t.Errorf("Server certificate was not communicated correctly")
				}
			})
		}
	})
}

func TestExtendedMasterSecret(t *testing.T) {
	// Check for leaking routines
	report := test.CheckRoutines(t)
	defer report()

	tests := map[string]struct {
		clientCfg         *Config
		serverCfg         *Config
		expectedClientErr error
		expectedServerErr error
	}{
		"Request_Request_ExtendedMasterSecret": {
			clientCfg: &Config{
				ExtendedMasterSecret: RequestExtendedMasterSecret,
			},
			serverCfg: &Config{
				ExtendedMasterSecret: RequestExtendedMasterSecret,
			},
			expectedClientErr: nil,
			expectedServerErr: nil,
		},
		"Request_Require_ExtendedMasterSecret": {
			clientCfg: &Config{
				ExtendedMasterSecret: RequestExtendedMasterSecret,
			},
			serverCfg: &Config{
				ExtendedMasterSecret: RequireExtendedMasterSecret,
			},
			expectedClientErr: nil,
			expectedServerErr: nil,
		},
		"Request_Disable_ExtendedMasterSecret": {
			clientCfg: &Config{
				ExtendedMasterSecret: RequestExtendedMasterSecret,
			},
			serverCfg: &Config{
				ExtendedMasterSecret: DisableExtendedMasterSecret,
			},
			expectedClientErr: nil,
			expectedServerErr: nil,
		},
		"Require_Request_ExtendedMasterSecret": {
			clientCfg: &Config{
				ExtendedMasterSecret: RequireExtendedMasterSecret,
			},
			serverCfg: &Config{
				ExtendedMasterSecret: RequestExtendedMasterSecret,
			},
			expectedClientErr: nil,
			expectedServerErr: nil,
		},
		"Require_Require_ExtendedMasterSecret": {
			clientCfg: &Config{
				ExtendedMasterSecret: RequireExtendedMasterSecret,
			},
			serverCfg: &Config{
				ExtendedMasterSecret: RequireExtendedMasterSecret,
			},
			expectedClientErr: nil,
			expectedServerErr: nil,
		},
		"Require_Disable_ExtendedMasterSecret": {
			clientCfg: &Config{
				ExtendedMasterSecret: RequireExtendedMasterSecret,
			},
			serverCfg: &Config{
				ExtendedMasterSecret: DisableExtendedMasterSecret,
			},
			expectedClientErr: errClientRequiredButNoServerEMS,
			expectedServerErr: &errAlert{&alert{alertLevelFatal, alertInsufficientSecurity}},
		},
		"Disable_Request_ExtendedMasterSecret": {
			clientCfg: &Config{
				ExtendedMasterSecret: DisableExtendedMasterSecret,
			},
			serverCfg: &Config{
				ExtendedMasterSecret: RequestExtendedMasterSecret,
			},
			expectedClientErr: nil,
			expectedServerErr: nil,
		},
		"Disable_Require_ExtendedMasterSecret": {
			clientCfg: &Config{
				ExtendedMasterSecret: DisableExtendedMasterSecret,
			},
			serverCfg: &Config{
				ExtendedMasterSecret: RequireExtendedMasterSecret,
			},
			expectedClientErr: &errAlert{&alert{alertLevelFatal, alertInsufficientSecurity}},
			expectedServerErr: errServerRequiredButNoClientEMS,
		},
		"Disable_Disable_ExtendedMasterSecret": {
			clientCfg: &Config{
				ExtendedMasterSecret: DisableExtendedMasterSecret,
			},
			serverCfg: &Config{
				ExtendedMasterSecret: DisableExtendedMasterSecret,
			},
			expectedClientErr: nil,
			expectedServerErr: nil,
		},
	}
	for name, tt := range tests {
		tt := tt
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			ca, cb := dpipe.Pipe()
			type result struct {
				c   *Conn
				err error
			}
			c := make(chan result)

			go func() {
				client, err := testClient(ctx, ca, tt.clientCfg, true)
				c <- result{client, err}
			}()

			server, err := testServer(ctx, cb, tt.serverCfg, true)
			res := <-c
			defer func() {
				if err == nil {
					_ = server.Close()
				}
				if res.err == nil {
					_ = res.c.Close()
				}
			}()

			if !errors.Is(res.err, tt.expectedClientErr) {
				t.Errorf("Client error expected: \"%v\" but got \"%v\"", tt.expectedClientErr, res.err)
			}

			if !errors.Is(err, tt.expectedServerErr) {
				t.Errorf("Server error expected: \"%v\" but got \"%v\"", tt.expectedServerErr, err)
			}
		})
	}
}

func TestServerCertificate(t *testing.T) {
	// Check for leaking routines
	report := test.CheckRoutines(t)
	defer report()

	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	caPool := x509.NewCertPool()
	caPool.AddCert(certificate)

	t.Run("parallel", func(t *testing.T) { // sync routines to check routine leak
		tests := map[string]struct {
			clientCfg *Config
			serverCfg *Config
			wantErr   bool
		}{
			"no_ca": {
				clientCfg: &Config{},
				serverCfg: &Config{Certificates: []tls.Certificate{cert}, ClientAuth: NoClientCert},
				wantErr:   true,
			},
			"good_ca": {
				clientCfg: &Config{RootCAs: caPool},
				serverCfg: &Config{Certificates: []tls.Certificate{cert}, ClientAuth: NoClientCert},
			},
			"no_ca_skip_verify": {
				clientCfg: &Config{InsecureSkipVerify: true},
				serverCfg: &Config{Certificates: []tls.Certificate{cert}, ClientAuth: NoClientCert},
			},
			"good_ca_skip_verify_custom_verify_peer": {
				clientCfg: &Config{RootCAs: caPool, Certificates: []tls.Certificate{cert}},
				serverCfg: &Config{Certificates: []tls.Certificate{cert}, ClientAuth: RequireAnyClientCert, VerifyPeerCertificate: func(cert [][]byte, chain [][]*x509.Certificate) error {
					if len(chain) != 0 {
						return errors.New("not expected chain")
					}
					return nil
				}},
			},
			"good_ca_verify_custom_verify_peer": {
				clientCfg: &Config{RootCAs: caPool, Certificates: []tls.Certificate{cert}},
				serverCfg: &Config{ClientCAs: caPool, Certificates: []tls.Certificate{cert}, ClientAuth: RequireAndVerifyClientCert, VerifyPeerCertificate: func(cert [][]byte, chain [][]*x509.Certificate) error {
					if len(chain) == 0 {
						return errors.New("expected chain")
					}
					return nil
				}},
			},
			"good_ca_custom_verify_peer": {
				clientCfg: &Config{
					RootCAs: caPool,
					VerifyPeerCertificate: func([][]byte, [][]*x509.Certificate) error {
						return errors.New("wrong cert")
					},
				},
				serverCfg: &Config{Certificates: []tls.Certificate{cert}, ClientAuth: NoClientCert},
				wantErr:   true,
			},
			"server_name": {
				clientCfg: &Config{RootCAs: caPool, ServerName: certificate.Subject.CommonName},
				serverCfg: &Config{Certificates: []tls.Certificate{cert}, ClientAuth: NoClientCert},
			},
			"server_name_error": {
				clientCfg: &Config{RootCAs: caPool, ServerName: "barfoo"},
				serverCfg: &Config{Certificates: []tls.Certificate{cert}, ClientAuth: NoClientCert},
				wantErr:   true,
			},
		}
		for name, tt := range tests {
			tt := tt
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				ca, cb := dpipe.Pipe()

				type result struct {
					c   *Conn
					err error
				}
				srvCh := make(chan result)
				go func() {
					s, err := Server(cb, tt.serverCfg)
					srvCh <- result{s, err}
				}()

				cli, err := Client(ca, tt.clientCfg)
				if err == nil {
					_ = cli.Close()
				}
				if !tt.wantErr && err != nil {
					t.Errorf("Client failed(%v)", err)
				}
				if tt.wantErr && err == nil {
					t.Fatal("Error expected")
				}

				srv := <-srvCh
				if srv.err == nil {
					_ = srv.c.Close()
				}
			})
		}
	})
}

func TestCipherSuiteConfiguration(t *testing.T) {
	// Check for leaking routines
	report := test.CheckRoutines(t)
	defer report()

	for _, test := range []struct {
		Name               string
		ClientCipherSuites []CipherSuiteID
		ServerCipherSuites []CipherSuiteID
		WantClientError    error
		WantServerError    error
	}{
		{
			Name:               "No CipherSuites specified",
			ClientCipherSuites: nil,
			ServerCipherSuites: nil,
			WantClientError:    nil,
			WantServerError:    nil,
		},
		{
			Name:               "Invalid CipherSuite",
			ClientCipherSuites: []CipherSuiteID{0x00},
			ServerCipherSuites: []CipherSuiteID{0x00},
			WantClientError:    &invalidCipherSuite{0x00},
			WantServerError:    &invalidCipherSuite{0x00},
		},
		{
			Name:               "Valid CipherSuites specified",
			ClientCipherSuites: []CipherSuiteID{TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
			ServerCipherSuites: []CipherSuiteID{TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
			WantClientError:    nil,
			WantServerError:    nil,
		},
		{
			Name:               "CipherSuites mismatch",
			ClientCipherSuites: []CipherSuiteID{TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
			ServerCipherSuites: []CipherSuiteID{TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA},
			WantClientError:    &errAlert{&alert{alertLevelFatal, alertInsufficientSecurity}},
			WantServerError:    errCipherSuiteNoIntersection,
		},
		{
			Name:               "Valid CipherSuites CCM specified",
			ClientCipherSuites: []CipherSuiteID{TLS_ECDHE_ECDSA_WITH_AES_128_CCM},
			ServerCipherSuites: []CipherSuiteID{TLS_ECDHE_ECDSA_WITH_AES_128_CCM},
			WantClientError:    nil,
			WantServerError:    nil,
		},
		{
			Name:               "Valid CipherSuites CCM-8 specified",
			ClientCipherSuites: []CipherSuiteID{TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8},
			ServerCipherSuites: []CipherSuiteID{TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8},
			WantClientError:    nil,
			WantServerError:    nil,
		},
	} {
		test := test
		t.Run(test.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			ca, cb := dpipe.Pipe()
			type result struct {
				c   *Conn
				err error
			}
			c := make(chan result)

			go func() {
				client, err := testClient(ctx, ca, &Config{CipherSuites: test.ClientCipherSuites}, true)
				c <- result{client, err}
			}()

			server, err := testServer(ctx, cb, &Config{CipherSuites: test.ServerCipherSuites}, true)
			if err == nil {
				defer func() {
					_ = server.Close()
				}()
			}
			if !errors.Is(err, test.WantServerError) {
				t.Errorf("TestCipherSuiteConfiguration: Server Error Mismatch '%s': expected(%v) actual(%v)", test.Name, test.WantServerError, err)
			}

			res := <-c
			if res.err == nil {
				_ = server.Close()
			}
			if !errors.Is(res.err, test.WantClientError) {
				t.Errorf("TestSRTPConfiguration: Client Error Mismatch '%s': expected(%v) actual(%v)", test.Name, test.WantClientError, res.err)
			}
		})
	}
}

func TestPSKConfiguration(t *testing.T) {
	// Check for leaking routines
	report := test.CheckRoutines(t)
	defer report()

	for _, test := range []struct {
		Name                 string
		ClientHasCertificate bool
		ServerHasCertificate bool
		ClientPSK            PSKCallback
		ServerPSK            PSKCallback
		ClientPSKIdentity    []byte
		ServerPSKIdentity    []byte
		WantClientError      error
		WantServerError      error
	}{
		{
			Name:                 "PSK specified",
			ClientHasCertificate: false,
			ServerHasCertificate: false,
			ClientPSK:            func([]byte) ([]byte, error) { return []byte{0x00, 0x01, 0x02}, nil },
			ServerPSK:            func([]byte) ([]byte, error) { return []byte{0x00, 0x01, 0x02}, nil },
			ClientPSKIdentity:    []byte{0x00},
			ServerPSKIdentity:    []byte{0x00},
			WantClientError:      errNoAvailableCipherSuites,
			WantServerError:      errNoAvailableCipherSuites,
		},
		{
			Name:                 "PSK and certificate specified",
			ClientHasCertificate: true,
			ServerHasCertificate: true,
			ClientPSK:            func([]byte) ([]byte, error) { return []byte{0x00, 0x01, 0x02}, nil },
			ServerPSK:            func([]byte) ([]byte, error) { return []byte{0x00, 0x01, 0x02}, nil },
			ClientPSKIdentity:    []byte{0x00},
			ServerPSKIdentity:    []byte{0x00},
			WantClientError:      errPSKAndCertificate,
			WantServerError:      errPSKAndCertificate,
		},
		{
			Name:                 "PSK and no identity specified",
			ClientHasCertificate: false,
			ServerHasCertificate: false,
			ClientPSK:            func([]byte) ([]byte, error) { return []byte{0x00, 0x01, 0x02}, nil },
			ServerPSK:            func([]byte) ([]byte, error) { return []byte{0x00, 0x01, 0x02}, nil },
			ClientPSKIdentity:    nil,
			ServerPSKIdentity:    nil,
			WantClientError:      errPSKAndIdentityMustBeSetForClient,
			WantServerError:      errNoAvailableCipherSuites,
		},
		{
			Name:                 "No PSK and identity specified",
			ClientHasCertificate: false,
			ServerHasCertificate: false,
			ClientPSK:            nil,
			ServerPSK:            nil,
			ClientPSKIdentity:    []byte{0x00},
			ServerPSKIdentity:    []byte{0x00},
			WantClientError:      errIdentityNoPSK,
			WantServerError:      errServerMustHaveCertificate,
		},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		ca, cb := dpipe.Pipe()
		type result struct {
			c   *Conn
			err error
		}
		c := make(chan result)

		go func() {
			client, err := testClient(ctx, ca, &Config{PSK: test.ClientPSK, PSKIdentityHint: test.ClientPSKIdentity}, test.ClientHasCertificate)
			c <- result{client, err}
		}()

		_, err := testServer(ctx, cb, &Config{PSK: test.ServerPSK, PSKIdentityHint: test.ServerPSKIdentity}, test.ServerHasCertificate)
		if err != nil || test.WantServerError != nil {
			if !(err != nil && test.WantServerError != nil && err.Error() == test.WantServerError.Error()) {
				t.Fatalf("TestPSKConfiguration: Server Error Mismatch '%s': expected(%v) actual(%v)", test.Name, test.WantServerError, err)
			}
		}

		res := <-c
		if res.err != nil || test.WantClientError != nil {
			if !(res.err != nil && test.WantClientError != nil && res.err.Error() == test.WantClientError.Error()) {
				t.Fatalf("TestPSKConfiguration: Client Error Mismatch '%s': expected(%v) actual(%v)", test.Name, test.WantClientError, res.err)
			}
		}
	}
}

func TestServerTimeout(t *testing.T) {
	// Limit runtime in case of deadlocks
	lim := test.TimeOut(time.Second * 20)
	defer lim.Stop()

	// Check for leaking routines
	report := test.CheckRoutines(t)
	defer report()

	cookie := make([]byte, 20)
	_, err := rand.Read(cookie)
	if err != nil {
		t.Fatal(err)
	}

	var rand [28]byte
	random := handshakeRandom{time.Unix(500, 0), rand}

	cipherSuites := []cipherSuite{
		&cipherSuiteTLSEcdheEcdsaWithAes128GcmSha256{},
		&cipherSuiteTLSEcdheRsaWithAes128GcmSha256{},
	}

	extensions := []extension{
		&extensionSupportedSignatureAlgorithms{
			signatureHashAlgorithms: []signatureHashAlgorithm{
				{hashAlgorithmSHA256, signatureAlgorithmECDSA},
				{hashAlgorithmSHA384, signatureAlgorithmECDSA},
				{hashAlgorithmSHA512, signatureAlgorithmECDSA},
				{hashAlgorithmSHA256, signatureAlgorithmRSA},
				{hashAlgorithmSHA384, signatureAlgorithmRSA},
				{hashAlgorithmSHA512, signatureAlgorithmRSA},
			},
		},
		&extensionSupportedEllipticCurves{
			ellipticCurves: []namedCurve{namedCurveX25519, namedCurveP256, namedCurveP384},
		},
		&extensionSupportedPointFormats{
			pointFormats: []ellipticCurvePointFormat{ellipticCurvePointFormatUncompressed},
		},
	}

	record := &recordLayer{
		recordLayerHeader: recordLayerHeader{
			sequenceNumber:  0,
			protocolVersion: protocolVersion1_2,
		},
		content: &handshake{
			// sequenceNumber and messageSequence line up, may need to be re-evaluated
			handshakeHeader: handshakeHeader{
				messageSequence: 0,
			},
			handshakeMessage: &handshakeMessageClientHello{
				version:            protocolVersion1_2,
				cookie:             cookie,
				random:             random,
				cipherSuites:       cipherSuites,
				compressionMethods: defaultCompressionMethods,
				extensions:         extensions,
			}},
	}

	packet, err := record.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	ca, cb := dpipe.Pipe()
	defer func() {
		err := ca.Close()
		if err != nil {
			t.Fatal(err)
		}
	}()

	// Client reader
	caReadChan := make(chan []byte, 1000)
	go func() {
		for {
			data := make([]byte, 8192)
			n, err := ca.Read(data)
			if err != nil {
				return
			}

			caReadChan <- data[:n]
		}
	}()

	// Start sending ClientHello packets until server responds with first packet
	go func() {
		for {
			select {
			case <-time.After(10 * time.Millisecond):
				_, err := ca.Write(packet)
				if err != nil {
					return
				}
			case <-caReadChan:
				// Once we receive the first reply from the server, stop
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	config := &Config{
		CipherSuites:   []CipherSuiteID{TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		FlightInterval: 100 * time.Millisecond,
	}

	_, serverErr := testServer(ctx, cb, config, true)
	if netErr, ok := serverErr.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("Client error exp(Temporary network error) failed(%v)", serverErr)
	}

	// Wait a little longer to ensure no additional messages have been sent by the server
	time.Sleep(300 * time.Millisecond)
	select {
	case msg := <-caReadChan:
		t.Fatalf("Expected no additional messages from server, got: %+v", msg)
	default:
	}
}

func TestProtocolVersionValidation(t *testing.T) {
	// Limit runtime in case of deadlocks
	lim := test.TimeOut(time.Second * 20)
	defer lim.Stop()

	// Check for leaking routines
	report := test.CheckRoutines(t)
	defer report()

	cookie := make([]byte, 20)
	if _, err := rand.Read(cookie); err != nil {
		t.Fatal(err)
	}

	var rand [28]byte
	random := handshakeRandom{time.Unix(500, 0), rand}

	localKeypair, err := generateKeypair(namedCurveX25519)
	if err != nil {
		t.Fatal(err)
	}

	config := &Config{
		CipherSuites:   []CipherSuiteID{TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		FlightInterval: 100 * time.Millisecond,
	}

	t.Run("Server", func(t *testing.T) {
		serverCases := map[string]struct {
			records []*recordLayer
		}{
			"ClientHelloVersion": {
				records: []*recordLayer{
					{
						recordLayerHeader: recordLayerHeader{
							protocolVersion: protocolVersion1_2,
						},
						content: &handshake{
							handshakeMessage: &handshakeMessageClientHello{
								version:            protocolVersion{0xfe, 0xff}, // try to downgrade
								cookie:             cookie,
								random:             random,
								cipherSuites:       []cipherSuite{&cipherSuiteTLSEcdheEcdsaWithAes128GcmSha256{}},
								compressionMethods: defaultCompressionMethods,
							}},
					},
				},
			},
			"SecondsClientHelloVersion": {
				records: []*recordLayer{
					{
						recordLayerHeader: recordLayerHeader{
							protocolVersion: protocolVersion1_2,
						},
						content: &handshake{
							handshakeMessage: &handshakeMessageClientHello{
								version:            protocolVersion1_2,
								cookie:             cookie,
								random:             random,
								cipherSuites:       []cipherSuite{&cipherSuiteTLSEcdheEcdsaWithAes128GcmSha256{}},
								compressionMethods: defaultCompressionMethods,
							}},
					},
					{
						recordLayerHeader: recordLayerHeader{
							protocolVersion: protocolVersion1_2,
							sequenceNumber:  1,
						},
						content: &handshake{
							handshakeHeader: handshakeHeader{
								messageSequence: 1,
							},
							handshakeMessage: &handshakeMessageClientHello{
								version:            protocolVersion{0xfe, 0xff}, // try to downgrade
								cookie:             cookie,
								random:             random,
								cipherSuites:       []cipherSuite{&cipherSuiteTLSEcdheEcdsaWithAes128GcmSha256{}},
								compressionMethods: defaultCompressionMethods,
							}},
					},
				},
			},
		}
		for name, c := range serverCases {
			c := c
			t.Run(name, func(t *testing.T) {
				ca, cb := dpipe.Pipe()
				defer func() {
					err := ca.Close()
					if err != nil {
						t.Error(err)
					}
				}()

				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()

				var wg sync.WaitGroup
				wg.Add(1)
				defer wg.Wait()
				go func() {
					defer wg.Done()
					if _, err := testServer(ctx, cb, config, true); !errors.Is(err, errUnsupportedProtocolVersion) {
						t.Errorf("Client error exp(%v) failed(%v)", errUnsupportedProtocolVersion, err)
					}
				}()

				time.Sleep(50 * time.Millisecond)

				resp := make([]byte, 1024)
				for _, record := range c.records {
					packet, err := record.Marshal()
					if err != nil {
						t.Fatal(err)
					}
					if _, werr := ca.Write(packet); werr != nil {
						t.Fatal(werr)
					}
					n, rerr := ca.Read(resp[:cap(resp)])
					if rerr != nil {
						t.Fatal(rerr)
					}
					resp = resp[:n]
				}

				h := &recordLayerHeader{}
				if err := h.Unmarshal(resp); err != nil {
					t.Fatal("Failed to unmarshal response")
				}
				if h.contentType != contentTypeAlert {
					t.Errorf("Peer must return alert to unsupported protocol version")
				}
			})
		}
	})

	t.Run("Client", func(t *testing.T) {
		clientCases := map[string]struct {
			records []*recordLayer
		}{
			"ServerHelloVersion": {
				records: []*recordLayer{
					{
						recordLayerHeader: recordLayerHeader{
							protocolVersion: protocolVersion1_2,
						},
						content: &handshake{
							handshakeMessage: &handshakeMessageHelloVerifyRequest{
								version: protocolVersion1_2,
								cookie:  cookie,
							}},
					},
					{
						recordLayerHeader: recordLayerHeader{
							protocolVersion: protocolVersion1_2,
							sequenceNumber:  1,
						},
						content: &handshake{
							handshakeHeader: handshakeHeader{
								messageSequence: 1,
							},
							handshakeMessage: &handshakeMessageServerHello{
								version:           protocolVersion{0xfe, 0xff}, // try to downgrade
								random:            random,
								cipherSuite:       &cipherSuiteTLSEcdheEcdsaWithAes128GcmSha256{},
								compressionMethod: defaultCompressionMethods[0],
							},
						}},
					{
						recordLayerHeader: recordLayerHeader{
							protocolVersion: protocolVersion1_2,
							sequenceNumber:  2,
						},
						content: &handshake{
							handshakeHeader: handshakeHeader{
								messageSequence: 2,
							},
							handshakeMessage: &handshakeMessageCertificate{},
						}},
					{
						recordLayerHeader: recordLayerHeader{
							protocolVersion: protocolVersion1_2,
							sequenceNumber:  3,
						},
						content: &handshake{
							handshakeHeader: handshakeHeader{
								messageSequence: 3,
							},
							handshakeMessage: &handshakeMessageServerKeyExchange{
								ellipticCurveType:  ellipticCurveTypeNamedCurve,
								namedCurve:         namedCurveX25519,
								publicKey:          localKeypair.publicKey,
								hashAlgorithm:      hashAlgorithmSHA256,
								signatureAlgorithm: signatureAlgorithmECDSA,
								signature:          make([]byte, 64),
							},
						}},
					{
						recordLayerHeader: recordLayerHeader{
							protocolVersion: protocolVersion1_2,
							sequenceNumber:  4,
						},
						content: &handshake{
							handshakeHeader: handshakeHeader{
								messageSequence: 4,
							},
							handshakeMessage: &handshakeMessageServerHelloDone{},
						}},
				},
			},
		}
		for name, c := range clientCases {
			c := c
			t.Run(name, func(t *testing.T) {
				ca, cb := dpipe.Pipe()
				defer func() {
					err := ca.Close()
					if err != nil {
						t.Error(err)
					}
				}()

				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()

				var wg sync.WaitGroup
				wg.Add(1)
				defer wg.Wait()
				go func() {
					defer wg.Done()
					if _, err := testClient(ctx, cb, config, true); !errors.Is(err, errUnsupportedProtocolVersion) {
						t.Errorf("Server error exp(%v) failed(%v)", errUnsupportedProtocolVersion, err)
					}
				}()

				time.Sleep(50 * time.Millisecond)

				for _, record := range c.records {
					if _, err := ca.Read(make([]byte, 1024)); err != nil {
						t.Fatal(err)
					}

					packet, err := record.Marshal()
					if err != nil {
						t.Fatal(err)
					}
					if _, err := ca.Write(packet); err != nil {
						t.Fatal(err)
					}
				}
				resp := make([]byte, 1024)
				n, err := ca.Read(resp)
				if err != nil {
					t.Fatal(err)
				}
				resp = resp[:n]

				h := &recordLayerHeader{}
				if err := h.Unmarshal(resp); err != nil {
					t.Fatal("Failed to unmarshal response")
				}
				if h.contentType != contentTypeAlert {
					t.Errorf("Peer must return alert to unsupported protocol version")
				}
			})
		}
	})
}
