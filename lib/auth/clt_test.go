/*
Copyright 2021 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509/pkix"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apiclient "github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/auth/testauthority"
	"github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/tlsca"
	"github.com/gravitational/trace"
	"github.com/stretchr/testify/require"
)

func TestClient_DialTimeout(t *testing.T) {
	t.Parallel()
	cases := []struct {
		desc    string
		timeout time.Duration
	}{
		{
			desc:    "dial timeout set to valid value",
			timeout: 500 * time.Millisecond,
		},
		{
			desc:    "defaults prevent infinite timeout",
			timeout: 0,
		},
	}

	for _, tt := range cases {
		t.Run(tt.desc, func(t *testing.T) {
			tt := tt
			t.Parallel()

			// create a client that will attempt to connect to a blackholed address. The address is reserved
			// for benchmarking by RFC 6890.
			cfg := apiclient.Config{
				DialTimeout: tt.timeout,
				Addrs:       []string{"198.18.0.254:1234"},
				Credentials: []apiclient.Credentials{
					apiclient.LoadTLS(&tls.Config{}),
				},
			}
			clt, err := NewClient(cfg)
			require.NoError(t, err)

			// call this so that the DialTimeout gets updated, if necessary, so that we know how long to
			// wait before failing this test
			require.NoError(t, cfg.CheckAndSetDefaults())

			errChan := make(chan error, 1)
			go func() {
				// try to create a session - this will timeout after the DialTimeout threshold is exceeded
				errChan <- clt.CreateSession(session.Session{Namespace: "test"})
			}()

			select {
			case err := <-errChan:
				require.Error(t, err)
			case <-time.After(cfg.DialTimeout + (cfg.DialTimeout / 2)):
				t.Fatal("Timed out waiting for dial to complete")
			}
		})
	}
}

func TestClient_RequestTimeout(t *testing.T) {
	t.Parallel()

	testDone := make(chan struct{})
	sawRoot := make(chan bool, 1)
	sawSlow := make(chan bool, 1)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/authorities/host/rotate/external" {
			sawRoot <- true
			http.Redirect(w, r, "/slow", http.StatusFound)
			return
		}

		if r.URL.Path == "/slow" {
			sawSlow <- true
			w.Write([]byte("Hello"))
			w.(http.Flusher).Flush()

			<-testDone
			return
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(testDone) }) // before srv.Close, to unblock /slow handler

	cfg := apiclient.Config{
		Addrs: []string{srv.Listener.Addr().String()},
		Credentials: []apiclient.Credentials{
			apiclient.LoadTLS(&tls.Config{
				InsecureSkipVerify: true,
			}),
		},
	}
	clt, err := NewClient(cfg)
	require.NoError(t, err)

	require.NotEmpty(t, cfg.Credentials)
	tlsCfg, err := cfg.Credentials[0].TLSConfig()
	require.NoError(t, err)
	srv.TLS = tlsCfg

	srv.StartTLS()

	ca := newCertAuthority(t, "test", types.HostCA)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	t.Cleanup(cancel)

	err = clt.RotateExternalCertAuthority(ctx, ca)
	require.ErrorIs(t, trace.Unwrap(err), context.DeadlineExceeded)

	select {
	case <-sawRoot:
		// good.
	default:
		t.Fatal("handler never got /v2/authorities/host/rotate/external request")
	}

	select {
	case <-sawSlow:
		// good.
	default:
		t.Fatal("handler never got /slow request")
	}
}

func newCertAuthority(t *testing.T, name string, caType types.CertAuthType) types.CertAuthority {
	ta := testauthority.New()
	priv, pub, err := ta.GenerateKeyPair("")
	require.NoError(t, err)

	// CA for cluster1 with 1 key pair.
	key, cert, err := tlsca.GenerateSelfSignedCA(pkix.Name{CommonName: name}, nil, time.Minute)
	require.NoError(t, err)

	ca, err := types.NewCertAuthority(types.CertAuthoritySpecV2{
		Type:        caType,
		ClusterName: name,
		ActiveKeys: types.CAKeySet{
			SSH: []*types.SSHKeyPair{{
				PrivateKey:     priv,
				PrivateKeyType: types.PrivateKeyType_RAW,
				PublicKey:      pub,
			}},
			TLS: []*types.TLSKeyPair{{
				Cert: cert,
				Key:  key,
			}},
		},
		Roles:      nil,
		SigningAlg: types.CertAuthoritySpecV2_RSA_SHA2_256,
	})
	require.NoError(t, err)
	return ca
}
