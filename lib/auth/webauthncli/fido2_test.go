//go:build libfido2
// +build libfido2

// Copyright 2022 Gravitational, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package webauthncli_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/duo-labs/webauthn/protocol"
	"github.com/duo-labs/webauthn/protocol/webauthncose"
	"github.com/fxamacker/cbor/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/lib/auth/mocku2f"
	"github.com/keys-pub/go-libfido2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	wanpb "github.com/gravitational/teleport/api/types/webauthn"
	wanlib "github.com/gravitational/teleport/lib/auth/webauthn"
	wancli "github.com/gravitational/teleport/lib/auth/webauthncli"
)

var makeCredentialAuthDataRaw, makeCredentialAuthDataCBOR, makeCredentialSig []byte
var assertionAuthDataRaw, assertionAuthDataCBOR, assertionSig []byte

func init() {
	// Initialize arrays with random data, but use realistic sizes.
	// YMMV.
	makeCredentialAuthDataRaw = make([]byte, 37)
	makeCredentialSig = make([]byte, 70)
	assertionAuthDataRaw = make([]byte, 37)
	assertionSig = make([]byte, 70)
	for _, b := range [][]byte{
		makeCredentialAuthDataRaw,
		makeCredentialSig,
		assertionAuthDataRaw,
		assertionSig,
	} {
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
	}

	// Returned authData is CBOR-encoded, so let's do that.
	pairs := []*[]byte{
		&makeCredentialAuthDataRaw, &makeCredentialAuthDataCBOR,
		&assertionAuthDataRaw, &assertionAuthDataCBOR,
	}
	for i := 0; i < len(pairs); i += 2 {
		dataRaw := pairs[i]
		dataCBOR := pairs[i+1]

		res, err := cbor.Marshal(*dataRaw)
		if err != nil {
			panic(err)
		}
		*dataCBOR = res
	}
}

// Capture common authenticator options.
var (
	authOpts = []libfido2.Option{
		{Name: "rk", Value: "true"},
		{Name: "up", Value: "true"},
		{Name: "plat", Value: "false"},
		{Name: "clientPin", Value: "false"}, // supported but unset
	}
	pinOpts = []libfido2.Option{
		{Name: "rk", Value: "true"},
		{Name: "up", Value: "true"},
		{Name: "plat", Value: "false"},
		{Name: "clientPin", Value: "true"}, // supported and configured
	}
	bioOpts = []libfido2.Option{
		{Name: "rk", Value: "true"},
		{Name: "up", Value: "true"},
		{Name: "uv", Value: "true"},
		{Name: "plat", Value: "false"},
		{Name: "alwaysUv", Value: "true"},
		{Name: "bioEnroll", Value: "true"}, // supported and configured
		{Name: "clientPin", Value: "true"}, // supported and configured
	}
)

// simplePicker is a credential picker that always picks the first credential.
type simplePicker struct{}

func (p simplePicker) PromptCredential(creds []*wancli.CredentialInfo) (*wancli.CredentialInfo, error) {
	return creds[0], nil
}

type noopPrompt struct {
	simplePicker
}

func (p noopPrompt) PromptPIN() (string, error) {
	return "", nil
}

func (p noopPrompt) PromptTouch() error { return nil }

// pinCancelPrompt exercises cancellation after device selection.
type pinCancelPrompt struct {
	simplePicker

	pin    string
	cancel context.CancelFunc
}

func (p *pinCancelPrompt) PromptPIN() (string, error) {
	p.cancel()
	return p.pin, nil
}

func (p pinCancelPrompt) PromptTouch() error {
	// 2nd touch never happens
	return nil
}

func TestIsFIDO2Available(t *testing.T) {
	const fido2Key = "TELEPORT_FIDO2"
	tests := []struct {
		name   string
		setenv func()
		want   bool
	}{
		{
			name: "env var unset",
			setenv: func() {
				os.Unsetenv(fido2Key)
			},
			want: true,
		},
		{
			name: "env var set to 1",
			setenv: func() {
				t.Setenv(fido2Key, "1")
			},
			want: true,
		},
		{
			name: "env var set to 0",
			setenv: func() {
				t.Setenv(fido2Key, "0")
			},
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.setenv()
			got := wancli.IsFIDO2Available()
			require.Equal(t, test.want, got, "IsFIDO2Available")
		})
	}
}

func TestFIDO2Login(t *testing.T) {
	resetFIDO2AfterTests(t)
	wancli.FIDO2PollInterval = 1 * time.Millisecond // run fast on tests

	const rpID = "example.com"
	const appID = "https://example.com"
	const origin = "https://example.com"

	// User IDs and names for resident credentials / passwordless.
	const llamaName = "llama"
	const alpacaName = "alpaca"
	var llamaID = make([]byte, 16)
	var alpacaID = make([]byte, 16)
	for _, b := range [][]byte{llamaID, alpacaID} {
		_, err := rand.Read(b)
		require.NoError(t, err, "Read failed")
	}

	// auth1 is a FIDO2 authenticator without a PIN configured.
	auth1 := mustNewFIDO2Device("/path1", "" /* pin */, &libfido2.DeviceInfo{
		Options: authOpts,
	})
	// pin1 is a FIDO2 authenticator with a PIN.
	pin1 := mustNewFIDO2Device("/pin1", "supersecretpinllama", &libfido2.DeviceInfo{
		Options: pinOpts,
	})
	// pin2 is a FIDO2 authenticator with a PIN.
	pin2 := mustNewFIDO2Device("/pin2", "supersecretpin2", &libfido2.DeviceInfo{
		Options: pinOpts,
	})
	// pin3 is a FIDO2 authenticator with a PIN and resident credentials.
	pin3 := mustNewFIDO2Device("/pin3", "supersecretpin3", &libfido2.DeviceInfo{
		Options: pinOpts,
	}, &libfido2.Credential{
		User: libfido2.User{
			ID:   alpacaID,
			Name: alpacaName,
		},
	})
	// bio1 is a biometric authenticator.
	bio1 := mustNewFIDO2Device("/bio1", "supersecretBIOpin", &libfido2.DeviceInfo{
		Options: bioOpts,
	})
	// bio2 is a biometric authenticator with configured resident credentials.
	bio2 := mustNewFIDO2Device("/bio2", "supersecretBIO2pin", &libfido2.DeviceInfo{
		Options: bioOpts,
	}, &libfido2.Credential{
		User: libfido2.User{
			ID:   llamaID,
			Name: llamaName,
		},
	}, &libfido2.Credential{
		User: libfido2.User{
			ID:   alpacaID,
			Name: alpacaName,
		},
	})
	// legacy1 is an authenticator registered using the U2F App ID.
	legacy1 := mustNewFIDO2Device("/legacy1", "" /* pin */, &libfido2.DeviceInfo{Options: authOpts})
	legacy1.wantRPID = appID

	challenge, err := protocol.CreateChallenge()
	require.NoError(t, err, "CreateChallenge failed")

	baseAssertion := &wanlib.CredentialAssertion{
		Response: protocol.PublicKeyCredentialRequestOptions{
			Challenge:          challenge,
			RelyingPartyID:     rpID,
			AllowedCredentials: []protocol.CredentialDescriptor{},
			UserVerification:   protocol.VerificationDiscouraged,
			Extensions:         map[string]interface{}{},
		},
	}

	tests := []struct {
		name            string
		timeout         time.Duration
		fido2           *fakeFIDO2
		setUP           func()
		createAssertion func() *wanlib.CredentialAssertion
		prompt          wancli.LoginPrompt
		opts            *wancli.LoginOpts
		// assertResponse and wantErr are mutually exclusive.
		assertResponse func(t *testing.T, resp *wanpb.CredentialAssertionResponse)
		wantErr        string
		wantUser       string
	}{
		{
			name:  "single device",
			fido2: newFakeFIDO2(auth1),
			setUP: func() {
				go func() {
					// Simulate delayed user press.
					time.Sleep(100 * time.Millisecond)
					auth1.setUP()
				}()
			},
			createAssertion: func() *wanlib.CredentialAssertion {
				cp := *baseAssertion
				cp.Response.AllowedCredentials = []protocol.CredentialDescriptor{
					{CredentialID: auth1.credentialID()},
				}
				return &cp
			},
			assertResponse: func(t *testing.T, resp *wanpb.CredentialAssertionResponse) {
				assert.Equal(t, auth1.credentialID(), resp.RawId, "RawId mismatch")
			},
		},
		{
			name:  "pin protected device",
			fido2: newFakeFIDO2(pin1),
			setUP: pin1.setUP,
			createAssertion: func() *wanlib.CredentialAssertion {
				cp := *baseAssertion
				cp.Response.AllowedCredentials = []protocol.CredentialDescriptor{
					{CredentialID: pin1.credentialID()},
				}
				return &cp
			},
		},
		{
			name:  "biometric device",
			fido2: newFakeFIDO2(bio1),
			setUP: bio1.setUP,
			createAssertion: func() *wanlib.CredentialAssertion {
				cp := *baseAssertion
				cp.Response.AllowedCredentials = []protocol.CredentialDescriptor{
					{CredentialID: bio1.credentialID()},
				}
				return &cp
			},
		},
		{
			name:  "legacy device (AppID)",
			fido2: newFakeFIDO2(legacy1),
			setUP: legacy1.setUP,
			createAssertion: func() *wanlib.CredentialAssertion {
				cp := *baseAssertion
				cp.Response.AllowedCredentials = []protocol.CredentialDescriptor{
					{CredentialID: legacy1.credentialID()},
				}
				cp.Response.Extensions = protocol.AuthenticationExtensions{
					wanlib.AppIDExtension: appID,
				}
				return &cp
			},
			assertResponse: func(t *testing.T, resp *wanpb.CredentialAssertionResponse) {
				assert.True(t, resp.Extensions.AppId, "AppID mismatch")
			},
		},
		{
			name: "multiple valid devices",
			fido2: newFakeFIDO2(
				auth1,
				pin1,
				bio1,
				legacy1,
			),
			setUP: bio1.setUP,
			createAssertion: func() *wanlib.CredentialAssertion {
				cp := *baseAssertion
				cp.Response.AllowedCredentials = []protocol.CredentialDescriptor{
					{CredentialID: auth1.credentialID()},
					{CredentialID: pin1.credentialID()},
					{CredentialID: bio1.credentialID()},
					{CredentialID: legacy1.credentialID()},
				}
				cp.Response.Extensions = protocol.AuthenticationExtensions{
					wanlib.AppIDExtension: appID,
				}
				return &cp
			},
			assertResponse: func(t *testing.T, resp *wanpb.CredentialAssertionResponse) {
				assert.Equal(t, bio1.credentialID(), resp.RawId, "RawId mismatch (want bio1)")
			},
		},
		{
			name: "multiple devices filtered",
			fido2: newFakeFIDO2(
				auth1, // allowed
				pin1,  // not allowed
				bio1,
				legacy1, // doesn't match RPID or AppID
			),
			setUP: auth1.setUP,
			createAssertion: func() *wanlib.CredentialAssertion {
				cp := *baseAssertion
				cp.Response.AllowedCredentials = []protocol.CredentialDescriptor{
					{CredentialID: auth1.credentialID()},
					{CredentialID: bio1.credentialID()},
					{CredentialID: legacy1.credentialID()},
				}
				cp.Response.Extensions = protocol.AuthenticationExtensions{
					wanlib.AppIDExtension: "https://badexample.com",
				}
				return &cp
			},
			assertResponse: func(t *testing.T, resp *wanpb.CredentialAssertionResponse) {
				assert.Equal(t, auth1.credentialID(), resp.RawId, "RawId mismatch (want auth1)")
			},
		},
		{
			name: "multiple pin devices",
			fido2: newFakeFIDO2(
				auth1,
				pin1, pin2,
				bio1,
			),
			setUP: pin2.setUP,
			createAssertion: func() *wanlib.CredentialAssertion {
				cp := *baseAssertion
				cp.Response.AllowedCredentials = []protocol.CredentialDescriptor{
					{CredentialID: auth1.credentialID()},
					{CredentialID: pin1.credentialID()},
					{CredentialID: pin2.credentialID()},
					{CredentialID: bio1.credentialID()},
				}
				return &cp
			},
			assertResponse: func(t *testing.T, resp *wanpb.CredentialAssertionResponse) {
				assert.Equal(t, pin2.credentialID(), resp.RawId, "RawId mismatch (want pin2)")
			},
		},
		{
			name:    "NOK no devices plugged times out",
			timeout: 10 * time.Millisecond,
			fido2:   newFakeFIDO2(),
			setUP:   func() {},
			createAssertion: func() *wanlib.CredentialAssertion {
				cp := *baseAssertion
				cp.Response.AllowedCredentials = []protocol.CredentialDescriptor{
					{CredentialID: auth1.credentialID()},
				}
				return &cp
			},
			wantErr: context.DeadlineExceeded.Error(),
		},
		{
			name:    "NOK no devices touched times out",
			timeout: 10 * time.Millisecond,
			fido2:   newFakeFIDO2(auth1, pin1, bio1, legacy1),
			setUP:   func() {}, // no interaction
			createAssertion: func() *wanlib.CredentialAssertion {
				cp := *baseAssertion
				cp.Response.AllowedCredentials = []protocol.CredentialDescriptor{
					{CredentialID: auth1.credentialID()},
					{CredentialID: pin1.credentialID()},
					{CredentialID: bio1.credentialID()},
				}
				return &cp
			},
			wantErr: context.DeadlineExceeded.Error(),
		},
		{
			name:    "NOK single candidate times out",
			timeout: 10 * time.Millisecond,
			fido2:   newFakeFIDO2(auth1, pin1),
			setUP:   func() {}, // no interaction
			createAssertion: func() *wanlib.CredentialAssertion {
				cp := *baseAssertion
				cp.Response.AllowedCredentials = []protocol.CredentialDescriptor{
					{CredentialID: auth1.credentialID()},
				}
				return &cp
			},
			wantErr: context.DeadlineExceeded.Error(),
		},
		{
			name:   "NOK cancel after PIN",
			fido2:  newFakeFIDO2(pin3, bio2),        // pin3 and bio2 have resident credentials
			setUP:  pin3.setUP,                      // user chooses pin3, but cancels before further touches
			prompt: &pinCancelPrompt{pin: pin3.pin}, // cancel set on test body
			createAssertion: func() *wanlib.CredentialAssertion {
				cp := *baseAssertion
				cp.Response.AllowedCredentials = nil // passwordless forces PIN
				cp.Response.UserVerification = protocol.VerificationRequired
				return &cp
			},
			wantErr: context.Canceled.Error(),
		},
		{
			name:  "passwordless pin",
			fido2: newFakeFIDO2(pin3),
			setUP: pin3.setUP,
			createAssertion: func() *wanlib.CredentialAssertion {
				cp := *baseAssertion
				cp.Response.AllowedCredentials = nil
				cp.Response.UserVerification = protocol.VerificationRequired
				return &cp
			},
			prompt: pin3,
			assertResponse: func(t *testing.T, resp *wanpb.CredentialAssertionResponse) {
				assert.Equal(t, pin3.credentials[0].ID, resp.RawId, "RawId mismatch (want %q resident credential)", alpacaName)
				assert.Equal(t, alpacaID, resp.Response.UserHandle, "UserHandle mismatch (want %q)", alpacaName)
			},
			wantUser: "", // single account response
		},
		{
			name:  "passwordless biometric (llama)",
			fido2: newFakeFIDO2(bio2),
			setUP: bio2.setUP,
			createAssertion: func() *wanlib.CredentialAssertion {
				cp := *baseAssertion
				cp.Response.AllowedCredentials = nil
				cp.Response.UserVerification = protocol.VerificationRequired
				return &cp
			},
			prompt: bio2,
			opts: &wancli.LoginOpts{
				User: llamaName,
			},
			assertResponse: func(t *testing.T, resp *wanpb.CredentialAssertionResponse) {
				assert.Equal(t, bio2.credentials[0].ID, resp.RawId, "RawId mismatch (want %q resident credential)", llamaName)
				assert.Equal(t, llamaID, resp.Response.UserHandle, "UserHandle mismatch (want %q)", llamaName)
			},
			wantUser: llamaName,
		},
		{
			name:  "passwordless biometric (alpaca)",
			fido2: newFakeFIDO2(bio2),
			setUP: bio2.setUP,
			createAssertion: func() *wanlib.CredentialAssertion {
				cp := *baseAssertion
				cp.Response.AllowedCredentials = nil
				cp.Response.UserVerification = protocol.VerificationRequired
				return &cp
			},
			prompt: bio2,
			opts: &wancli.LoginOpts{
				User: alpacaName,
			},
			assertResponse: func(t *testing.T, resp *wanpb.CredentialAssertionResponse) {
				assert.Equal(t, bio2.credentials[1].ID, resp.RawId, "RawId mismatch (want %q resident credential)", alpacaName)
				assert.Equal(t, alpacaID, resp.Response.UserHandle, "UserHandle mismatch (want %q)", alpacaName)
			},
			wantUser: alpacaName,
		},
		{
			name:  "passwordless single-choice credential picker",
			fido2: newFakeFIDO2(pin3),
			setUP: pin3.setUP,
			createAssertion: func() *wanlib.CredentialAssertion {
				cp := *baseAssertion
				cp.Response.AllowedCredentials = nil
				cp.Response.UserVerification = protocol.VerificationRequired
				return &cp
			},
			prompt: pin3,
			assertResponse: func(t *testing.T, resp *wanpb.CredentialAssertionResponse) {
				assert.Equal(t, pin3.credentials[0].ID, resp.RawId, "RawId mismatch (want %q resident credential)", alpacaName)
				assert.Equal(t, alpacaID, resp.Response.UserHandle, "UserHandle mismatch (want %q)", alpacaName)
			},
			wantUser: "", // single account response
		},
		{
			name:  "passwordless multi-choice credential picker",
			fido2: newFakeFIDO2(bio2),
			setUP: bio2.setUP,
			createAssertion: func() *wanlib.CredentialAssertion {
				cp := *baseAssertion
				cp.Response.AllowedCredentials = nil
				cp.Response.UserVerification = protocol.VerificationRequired
				return &cp
			},
			prompt: bio2, // picks first credential from list.
			assertResponse: func(t *testing.T, resp *wanpb.CredentialAssertionResponse) {
				assert.Equal(t, bio2.credentials[0].ID, resp.RawId, "RawId mismatch (want %q resident credential)", llamaName)
				assert.Equal(t, llamaID, resp.Response.UserHandle, "UserHandle mismatch (want %q)", llamaName)
			},
			wantUser: llamaName,
		},
		{
			name:  "NOK passwordless no credentials",
			fido2: newFakeFIDO2(bio1),
			setUP: bio1.setUP,
			createAssertion: func() *wanlib.CredentialAssertion {
				cp := *baseAssertion
				cp.Response.AllowedCredentials = nil
				cp.Response.UserVerification = protocol.VerificationRequired
				return &cp
			},
			prompt:  bio1,
			wantErr: libfido2.ErrNoCredentials.Error(),
		},
		{
			name:  "NOK passwordless unknown user",
			fido2: newFakeFIDO2(bio2),
			setUP: bio2.setUP,
			createAssertion: func() *wanlib.CredentialAssertion {
				cp := *baseAssertion
				cp.Response.AllowedCredentials = nil
				cp.Response.UserVerification = protocol.VerificationRequired
				return &cp
			},
			prompt: bio2,
			opts: &wancli.LoginOpts{
				User: "camel", // unknown
			},
			wantErr: "no credentials for user",
		},
	}
	for _, test := range tests {
		runTest := func(t *testing.T, f2 *fakeFIDO2) {
			f2.setCallbacks()
			test.setUP()

			timeout := test.timeout
			if timeout == 0 {
				timeout = 1 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			prompt := test.prompt
			if prompt == nil {
				prompt = noopPrompt{}
			}
			if pp, ok := prompt.(*pinCancelPrompt); ok {
				pp.cancel = cancel
			}

			// Run FIDO2Login asynchronously, so we can fail the test if it hangs.
			// mfaResp and err checked below.
			var mfaResp *proto.MFAAuthenticateResponse
			var actualUser string
			var err error
			done := make(chan struct{})
			go func() {
				mfaResp, actualUser, err = wancli.FIDO2Login(ctx, origin, test.createAssertion(), prompt, test.opts)
				close(done)
			}()
			select {
			case <-done: // OK, proceed.
			case <-time.After(timeout + 1*time.Second):
				t.Fatal("Timed out waiting for FIDO2Login")
			}

			switch {
			case test.wantErr != "" && err == nil:
				t.Fatalf("FIDO2Login returned err = nil, wantErr %q", test.wantErr)
			case test.wantErr != "":
				require.Contains(t, err.Error(), test.wantErr, "FIDO2Login returned err = %q, wantErr %q", err, test.wantErr)
				return
			default:
				require.NoError(t, err, "FIDO2Login failed")
				require.NotNil(t, mfaResp, "mfaResp nil")
			}

			// Do a few baseline checks, tests can assert further.
			got := mfaResp.GetWebauthn()
			require.NotNil(t, got, "assertion response nil")
			require.NotNil(t, got.Response, "authenticator response nil")
			assert.NotNil(t, got.Response.ClientDataJson, "ClientDataJSON nil")
			want := &wanpb.CredentialAssertionResponse{
				Type:  string(protocol.PublicKeyCredentialType),
				RawId: got.RawId,
				Response: &wanpb.AuthenticatorAssertionResponse{
					ClientDataJson:    got.Response.ClientDataJson,
					AuthenticatorData: assertionAuthDataRaw,
					Signature:         assertionSig,
					UserHandle:        got.Response.UserHandle,
				},
				Extensions: got.Extensions,
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("FIDO2Login()/CredentialAssertionResponse mismatch (-want +got):\n%v", diff)
			}

			if test.assertResponse != nil {
				test.assertResponse(t, got)
			}

			assert.Equal(t, test.wantUser, actualUser, "actual user mismatch")
		}

		// Run tests against both "metered" and "non-metered" fake variants, so we
		// can ensure both behave correctly.
		// There shouldn't be much of a difference, but tests are fast enough that
		// it doesn't hurt either.
		t.Run(test.name+"/metered", func(t *testing.T) {
			runTest(t, test.fido2)
		})
		t.Run(test.name+"/nonMetered", func(t *testing.T) {
			runTest(t, test.fido2.withNonMeteredLocations())
		})
	}
}

func TestFIDO2Login_retryUVFailures(t *testing.T) {
	resetFIDO2AfterTests(t)

	const user = "llama"
	pin1 := mustNewFIDO2Device("/pin1", "supersecretpinllama", &libfido2.DeviceInfo{
		Options: pinOpts,
	}, &libfido2.Credential{
		ID: []byte{1, 1, 1, 1, 1},
		User: libfido2.User{
			ID:   []byte{1, 1, 1, 1, 2},
			Name: user,
		},
	})
	pin1.failUV = true // fail UV regardless of PIN

	f2 := newFakeFIDO2(pin1).withNonMeteredLocations()
	f2.setCallbacks()

	const rpID = "example.com"
	const origin = "https://example.com"
	ctx := context.Background()
	assertion := &wanlib.CredentialAssertion{
		Response: protocol.PublicKeyCredentialRequestOptions{
			Challenge:        []byte{1, 2, 3, 4, 5}, // arbitrary
			RelyingPartyID:   rpID,
			UserVerification: protocol.VerificationRequired,
		},
	}

	pin1.setUP()
	_, _, err := wancli.FIDO2Login(ctx, origin, assertion, pin1 /* prompt */, nil /* opts */)
	require.NoError(t, err, "FIDO2Login failed UV retry")
}

func TestFIDO2Login_singleResidentCredential(t *testing.T) {
	resetFIDO2AfterTests(t)

	const user1Name = "llama"
	const user2Name = "alpaca"
	user1ID := []byte{1, 1, 1, 1, 1}
	user2ID := []byte{1, 1, 1, 1, 2}

	oneCredential := mustNewFIDO2Device("/bio1", "supersecretBIO1pin", &libfido2.DeviceInfo{
		Options: bioOpts,
	}, &libfido2.Credential{
		ID: []byte{1, 1, 1, 1, 1},
		User: libfido2.User{
			ID:   user1ID,
			Name: user1Name,
		},
	})
	manyCredentials := mustNewFIDO2Device("/bio2", "supersecretBIO2pin", &libfido2.DeviceInfo{
		Options: bioOpts,
	},
		&libfido2.Credential{
			ID: user1ID,
			User: libfido2.User{
				ID:   user1ID,
				Name: user1Name,
			},
		},
		&libfido2.Credential{
			ID: user2ID,
			User: libfido2.User{
				ID:   user2ID,
				Name: user2Name,
			},
		})

	f2 := newFakeFIDO2(oneCredential, manyCredentials).withNonMeteredLocations()
	f2.setCallbacks()

	const rpID = "example.com"
	const origin = "https://example.com"
	ctx := context.Background()
	assertion := &wanlib.CredentialAssertion{
		Response: protocol.PublicKeyCredentialRequestOptions{
			Challenge:        []byte{1, 2, 3, 4, 5}, // arbitrary
			RelyingPartyID:   rpID,
			UserVerification: protocol.VerificationRequired,
		},
	}

	tests := []struct {
		name       string
		up         func()
		prompt     wancli.LoginPrompt
		opts       *wancli.LoginOpts
		wantUserID []byte
		// Actual user is empty for all single account cases.
		// Authenticators don't return the data.
		wantUser string
	}{
		{
			name:       "single credential with empty user",
			up:         oneCredential.setUP,
			prompt:     oneCredential,
			wantUserID: user1ID,
		},
		{
			name:   "single credential with correct user",
			up:     oneCredential.setUP,
			prompt: oneCredential,
			opts: &wancli.LoginOpts{
				User: user1Name, // happens to match
			},
			wantUserID: user1ID,
		},
		{
			name:   "single credential with ignored user",
			up:     oneCredential.setUP,
			prompt: oneCredential,
			opts: &wancli.LoginOpts{
				User: user2Name, // ignored, we just can't know
			},
			wantUserID: user1ID,
		},
		{
			name:   "multi credentials",
			up:     manyCredentials.setUP,
			prompt: manyCredentials,
			opts: &wancli.LoginOpts{
				User: user2Name, // respected, authenticator returns the data
			},
			wantUserID: user2ID,
			wantUser:   user2Name,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.up()

			resp, gotUser, err := wancli.FIDO2Login(ctx, origin, assertion, test.prompt, test.opts)
			require.NoError(t, err, "FIDO2Login failed")
			gotUserID := resp.GetWebauthn().GetResponse().GetUserHandle()
			assert.Equal(t, test.wantUserID, gotUserID, "FIDO2Login user ID mismatch")
			assert.Equal(t, test.wantUser, gotUser, "FIDO2Login user mismatch")
		})
	}
}

type countingPrompt struct {
	wancli.LoginPrompt
	count int
}

func (cp *countingPrompt) PromptTouch() error {
	cp.count++
	return cp.LoginPrompt.PromptTouch()
}

func TestFIDO2Login_PromptTouch(t *testing.T) {
	resetFIDO2AfterTests(t)

	const rpID = "example.com"
	const origin = "https://example.com"

	// auth1 is a FIDO2 authenticator without a PIN configured.
	auth1 := mustNewFIDO2Device("/auth1", "" /* pin */, &libfido2.DeviceInfo{
		Options: authOpts,
	})
	// pin1 is a FIDO2 authenticator with a PIN and resident credentials.
	pin1 := mustNewFIDO2Device("/pin1", "supersecretpin1", &libfido2.DeviceInfo{
		Options: pinOpts,
	}, &libfido2.Credential{
		ID: []byte{1, 1, 1, 1},
		User: libfido2.User{
			ID:   []byte("alpacaID"),
			Name: "alpaca",
		},
	})
	// bio1 is a biometric authenticator with configured resident credentials.
	bio1 := mustNewFIDO2Device("/bio1", "supersecretBIO1pin", &libfido2.DeviceInfo{
		Options: bioOpts,
	}, &libfido2.Credential{
		ID: []byte{1, 1, 1, 2},
		User: libfido2.User{
			ID:   []byte("llamaID"),
			Name: "llama",
		},
	}, &libfido2.Credential{
		ID: []byte{1, 1, 1, 3},
		User: libfido2.User{
			ID:   []byte("alpacaID"),
			Name: "alpaca",
		},
	})

	mfaAssertion := &wanlib.CredentialAssertion{
		Response: protocol.PublicKeyCredentialRequestOptions{
			Challenge:      make([]byte, 32),
			RelyingPartyID: rpID,
			AllowedCredentials: []protocol.CredentialDescriptor{
				{
					Type:         protocol.PublicKeyCredentialType,
					CredentialID: auth1.credentialID(),
				},
				{
					Type:         protocol.PublicKeyCredentialType,
					CredentialID: pin1.credentialID(),
				},
				{
					Type:         protocol.PublicKeyCredentialType,
					CredentialID: bio1.credentialID(),
				},
			},
		},
	}
	pwdlessAssertion := &wanlib.CredentialAssertion{
		Response: protocol.PublicKeyCredentialRequestOptions{
			Challenge:        make([]byte, 32),
			RelyingPartyID:   rpID,
			UserVerification: protocol.VerificationRequired,
		},
	}

	tests := []struct {
		name        string
		fido2       *fakeFIDO2
		assertion   *wanlib.CredentialAssertion
		prompt      wancli.LoginPrompt
		opts        *wancli.LoginOpts
		wantTouches int
	}{
		{
			name:        "MFA requires single touch",
			fido2:       newFakeFIDO2(auth1, pin1, bio1),
			assertion:   mfaAssertion,
			prompt:      auth1,
			wantTouches: 1,
		},
		{
			name:        "Passwordless PIN plugged requires two touches",
			fido2:       newFakeFIDO2(pin1).withNonMeteredLocations(),
			assertion:   pwdlessAssertion,
			prompt:      pin1,
			wantTouches: 2,
		},
		{
			name:        "Passwordless PIN not plugged requires two touches",
			fido2:       newFakeFIDO2(pin1),
			assertion:   pwdlessAssertion,
			prompt:      pin1,
			wantTouches: 2,
		},
		{
			name:      "Passwordless Bio requires one touch",
			fido2:     newFakeFIDO2(bio1),
			assertion: pwdlessAssertion,
			prompt:    bio1,
			opts: &wancli.LoginOpts{
				User: "llama",
			},
			wantTouches: 1,
		},
		{
			name:        "Passwordless with multiple devices requires two touches",
			fido2:       newFakeFIDO2(pin1, bio1),
			assertion:   pwdlessAssertion,
			prompt:      pin1,
			wantTouches: 2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.fido2.setCallbacks()

			// Set a timeout, just in case.
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()

			prompt := &countingPrompt{LoginPrompt: test.prompt}
			_, _, err := wancli.FIDO2Login(ctx, origin, test.assertion, prompt, test.opts)
			require.NoError(t, err, "FIDO2Login errored")
			assert.Equal(t, test.wantTouches, prompt.count, "FIDO2Login did an unexpected number of touch prompts")
		})
	}
}

func TestFIDO2Login_u2fDevice(t *testing.T) {
	resetFIDO2AfterTests(t)

	dev := mustNewFIDO2Device("/u2f", "" /* pin */, nil /* info */)
	dev.u2fOnly = true

	f2 := newFakeFIDO2(dev).withNonMeteredLocations()
	f2.setCallbacks()

	const rpID = "example.com"
	const origin = "https://example.com"

	// Set a ctx timeout in case something goes wrong.
	// Under normal circumstances the test gets nowhere near this timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cc := &wanlib.CredentialCreation{
		Response: protocol.PublicKeyCredentialCreationOptions{
			Challenge: []byte{1, 2, 3, 4, 5}, // arbitrary
			RelyingParty: protocol.RelyingPartyEntity{
				ID: rpID,
			},
			Parameters: []protocol.CredentialParameter{
				{
					Type:      protocol.PublicKeyCredentialType,
					Algorithm: webauthncose.AlgES256,
				},
			},
			AuthenticatorSelection: protocol.AuthenticatorSelection{
				UserVerification: protocol.VerificationDiscouraged,
			},
			Attestation: protocol.PreferNoAttestation,
		},
	}

	dev.setUP() // simulate touch
	ccr, err := wancli.FIDO2Register(ctx, origin, cc, dev /* prompt */)
	require.NoError(t, err, "FIDO2Register errored")

	assertion := &wanlib.CredentialAssertion{
		Response: protocol.PublicKeyCredentialRequestOptions{
			Challenge:      []byte{1, 2, 3, 4, 5}, // arbitrary
			RelyingPartyID: rpID,
			AllowedCredentials: []protocol.CredentialDescriptor{
				{
					Type:         protocol.PublicKeyCredentialType,
					CredentialID: ccr.GetWebauthn().GetRawId(),
				},
			},
			UserVerification: protocol.VerificationDiscouraged,
		},
	}

	dev.setUP() // simulate touch
	_, _, err = wancli.FIDO2Login(ctx, origin, assertion, dev /* prompt */, nil /* opts */)
	assert.NoError(t, err, "FIDO2Login errored")
}

func TestFIDO2Login_bioErrorHandling(t *testing.T) {
	resetFIDO2AfterTests(t)

	// bio is a biometric authenticator with configured resident credentials.
	bio := mustNewFIDO2Device("/bio", "supersecretBIOpin", &libfido2.DeviceInfo{
		Options: bioOpts,
	}, &libfido2.Credential{
		User: libfido2.User{
			ID:   []byte{1, 2, 3, 4, 5}, // unimportant
			Name: "llama",
		},
	})

	f2 := newFakeFIDO2(bio).withNonMeteredLocations()
	f2.setCallbacks()

	// Prepare a passwordless assertion.
	// MFA would do as well; both are realistic here.
	const origin = "https://example.com"
	assertion := &wanlib.CredentialAssertion{
		Response: protocol.PublicKeyCredentialRequestOptions{
			Challenge:          []byte{1, 2, 3, 4, 5},
			RelyingPartyID:     "example.com",
			AllowedCredentials: nil,                           // passwordless
			UserVerification:   protocol.VerificationRequired, // passwordless
		},
	}

	tests := []struct {
		name               string
		setAssertionErrors func()
		wantMsg            string
	}{
		{
			name:               "success (sanity check)",
			setAssertionErrors: func() { bio.assertionErrors = nil },
		},
		{
			name: "libfido2 error 60 fails with custom message",
			setAssertionErrors: func() {
				bio.assertionErrors = []error{
					libfido2.Error{Code: 60},
				}
			},
			wantMsg: "user verification function",
		},
		{
			name: "libfido2 error 63 retried",
			setAssertionErrors: func() {
				bio.assertionErrors = []error{
					libfido2.Error{Code: 63},
					libfido2.Error{Code: 63},
				}
			},
		},
		{
			name: "error retry has a limit",
			setAssertionErrors: func() {
				bio.assertionErrors = []error{
					libfido2.Error{Code: 63},
					libfido2.Error{Code: 63},
					libfido2.Error{Code: 63},
					libfido2.Error{Code: 63},
					libfido2.Error{Code: 63},
				}
			},
			wantMsg: "libfido2 error 63",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.setAssertionErrors()

			// Use a ctx with timeout just to be safe. We shouldn't hit the timeout.
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()

			_, _, err := wancli.FIDO2Login(ctx, origin, assertion, bio /* prompt */, nil /* opts */)
			if test.wantMsg == "" {
				require.NoError(t, err, "FIDO2Login returned non-nil error")
			} else {
				require.ErrorContains(t, err, test.wantMsg, "FIDO2Login returned an unexpected error")
			}
		})
	}
}

func TestFIDO2Login_errors(t *testing.T) {
	resetFIDO2AfterTests(t)

	// Make sure we won't call the real libfido2.
	f2 := newFakeFIDO2()
	f2.setCallbacks()

	const origin = "https://example.com"
	okAssertion := &wanlib.CredentialAssertion{
		Response: protocol.PublicKeyCredentialRequestOptions{
			Challenge:      make([]byte, 32),
			RelyingPartyID: "example.com",
			AllowedCredentials: []protocol.CredentialDescriptor{
				{Type: protocol.PublicKeyCredentialType, CredentialID: []byte{1, 2, 3, 4, 5}},
			},
		},
	}
	var prompt noopPrompt

	nilChallengeAssertion := *okAssertion
	nilChallengeAssertion.Response.Challenge = nil

	emptyRPIDAssertion := *okAssertion
	emptyRPIDAssertion.Response.RelyingPartyID = ""

	tests := []struct {
		name      string
		origin    string
		assertion *wanlib.CredentialAssertion
		prompt    wancli.LoginPrompt
		wantErr   string
	}{
		{
			name:      "ok - timeout", // check that good params are good
			origin:    origin,
			assertion: okAssertion,
			prompt:    prompt,
			wantErr:   context.DeadlineExceeded.Error(),
		},
		{
			name:      "nil origin",
			assertion: okAssertion,
			prompt:    prompt,
			wantErr:   "origin",
		},
		{
			name:    "nil assertion",
			origin:  origin,
			prompt:  prompt,
			wantErr: "assertion required",
		},
		{
			name:      "assertion without challenge",
			origin:    origin,
			assertion: &nilChallengeAssertion,
			prompt:    prompt,
			wantErr:   "challenge",
		},
		{
			name:      "assertion without RPID",
			origin:    origin,
			assertion: &emptyRPIDAssertion,
			prompt:    prompt,
			wantErr:   "relying party ID",
		},
		{
			name:      "nil prompt",
			origin:    origin,
			assertion: okAssertion,
			wantErr:   "prompt",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
			defer cancel()

			_, _, err := wancli.FIDO2Login(ctx, test.origin, test.assertion, test.prompt, nil /* opts */)
			require.Error(t, err, "FIDO2Login returned err = nil, want %q", test.wantErr)
			assert.Contains(t, err.Error(), test.wantErr, "FIDO2Login returned err = %q, want %q", err, test.wantErr)
		})
	}
}

func TestFIDO2Register(t *testing.T) {
	resetFIDO2AfterTests(t)

	const rpID = "example.com"
	const origin = "https://example.com"

	// auth1 is a FIDO2 authenticator without a PIN configured.
	auth1 := mustNewFIDO2Device("/path1", "" /* pin */, &libfido2.DeviceInfo{
		Options: authOpts,
	})
	// pin1 is a FIDO2 authenticator with a PIN.
	pin1 := mustNewFIDO2Device("/pin1", "supersecretpinllama", &libfido2.DeviceInfo{
		Options: pinOpts,
	})
	// pin2 is a FIDO2 authenticator with a PIN.
	pin2 := mustNewFIDO2Device("/pin2", "supersecretpin2", &libfido2.DeviceInfo{
		Options: pinOpts,
	})
	// bio1 is a biometric authenticator.
	bio1 := mustNewFIDO2Device("/bio1", "supersecretBIOpin", &libfido2.DeviceInfo{
		Options: []libfido2.Option{
			{Name: "rk", Value: "true"},
			{Name: "up", Value: "true"},
			{Name: "uv", Value: "true"},
			{Name: "plat", Value: "false"},
			{Name: "alwaysUv", Value: "true"},
			{Name: "bioEnroll", Value: "true"}, // supported and configured
			{Name: "clientPin", Value: "true"}, // supported and configured
		},
	})
	// u2f1 is an authenticator that uses fido-u2f attestation.
	u2f1 := mustNewFIDO2Device("/u2f1", "" /* pin */, &libfido2.DeviceInfo{Options: authOpts})
	u2f1.format = "fido-u2f"
	// none1 is an authenticator that returns no attestation data.
	none1 := mustNewFIDO2Device("/none1", "" /* pin */, &libfido2.DeviceInfo{Options: authOpts})
	none1.format = "none"

	challenge, err := protocol.CreateChallenge()
	require.NoError(t, err, "CreateChallenge failed")

	baseCC := &wanlib.CredentialCreation{
		Response: protocol.PublicKeyCredentialCreationOptions{
			Challenge: challenge,
			RelyingParty: protocol.RelyingPartyEntity{
				ID: rpID,
			},
			Parameters: []protocol.CredentialParameter{
				{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgES256},
			},
			AuthenticatorSelection: protocol.AuthenticatorSelection{
				UserVerification: protocol.VerificationDiscouraged,
			},
			Attestation: protocol.PreferDirectAttestation,
		},
	}
	pwdlessCC := *baseCC
	pwdlessCC.Response.RelyingParty.Name = "Teleport"
	pwdlessCC.Response.User = protocol.UserEntity{
		CredentialEntity: protocol.CredentialEntity{
			Name: "llama",
		},
		DisplayName: "Llama",
		ID:          []byte{1, 2, 3, 4, 5}, // arbitrary
	}
	pwdlessRRK := true
	pwdlessCC.Response.AuthenticatorSelection.RequireResidentKey = &pwdlessRRK
	pwdlessCC.Response.AuthenticatorSelection.UserVerification = protocol.VerificationRequired

	tests := []struct {
		name             string
		timeout          time.Duration
		fido2            *fakeFIDO2
		setUP            func()
		createCredential func() *wanlib.CredentialCreation
		prompt           wancli.RegisterPrompt
		wantErr          error
		assertResponse   func(t *testing.T, ccr *wanpb.CredentialCreationResponse, attObj *protocol.AttestationObject)
	}{
		{
			name:  "single device, packed attestation",
			fido2: newFakeFIDO2(auth1),
			setUP: auth1.setUP,
			createCredential: func() *wanlib.CredentialCreation {
				cp := *baseCC
				return &cp
			},
			assertResponse: func(t *testing.T, ccr *wanpb.CredentialCreationResponse, attObj *protocol.AttestationObject) {
				assert.Equal(t, auth1.credentialID(), ccr.RawId, "RawId mismatch")

				// Assert attestation algorithm and signature.
				require.Equal(t, "packed", attObj.Format, "attestation format mismatch")
				assert.Equal(t, int64(webauthncose.AlgES256), attObj.AttStatement["alg"], "attestation alg mismatch")
				assert.Equal(t, makeCredentialSig, attObj.AttStatement["sig"], "attestation sig mismatch")

				// Assert attestation certificate.
				x5cInterface := attObj.AttStatement["x5c"]
				x5c, ok := x5cInterface.([]interface{})
				require.True(t, ok, "attestation x5c type mismatch (got %T)", x5cInterface)
				assert.Len(t, x5c, 1, "attestation x5c length mismatch")
				assert.Equal(t, auth1.cert(), x5c[0], "attestation cert mismatch")
			},
		},
		{
			name:  "fido-u2f attestation",
			fido2: newFakeFIDO2(u2f1),
			setUP: u2f1.setUP,
			createCredential: func() *wanlib.CredentialCreation {
				cp := *baseCC
				return &cp
			},
			assertResponse: func(t *testing.T, ccr *wanpb.CredentialCreationResponse, attObj *protocol.AttestationObject) {
				// Assert attestation signature.
				require.Equal(t, "fido-u2f", attObj.Format, "attestation format mismatch")
				assert.Equal(t, makeCredentialSig, attObj.AttStatement["sig"], "attestation sig mismatch")

				// Assert attestation certificate.
				x5cInterface := attObj.AttStatement["x5c"]
				x5c, ok := x5cInterface.([]interface{})
				require.True(t, ok, "attestation x5c type mismatch (got %T)", x5cInterface)
				assert.Len(t, x5c, 1, "attestation x5c length mismatch")
				assert.Equal(t, u2f1.cert(), x5c[0], "attestation cert mismatch")
			},
		},
		{
			name:  "none attestation",
			fido2: newFakeFIDO2(none1),
			setUP: none1.setUP,
			createCredential: func() *wanlib.CredentialCreation {
				cp := *baseCC
				return &cp
			},
			assertResponse: func(t *testing.T, ccr *wanpb.CredentialCreationResponse, attObj *protocol.AttestationObject) {
				assert.Equal(t, "none", attObj.Format, "attestation format mismatch")
			},
		},
		{
			name:  "pin device",
			fido2: newFakeFIDO2(pin1),
			setUP: pin1.setUP,
			createCredential: func() *wanlib.CredentialCreation {
				cp := *baseCC
				return &cp
			},
			prompt: pin1,
		},
		{
			name:  "multiple valid devices",
			fido2: newFakeFIDO2(auth1, pin1, pin2, bio1),
			setUP: bio1.setUP,
			createCredential: func() *wanlib.CredentialCreation {
				cp := *baseCC
				return &cp
			},
			assertResponse: func(t *testing.T, ccr *wanpb.CredentialCreationResponse, attObj *protocol.AttestationObject) {
				assert.Equal(t, bio1.credentialID(), ccr.RawId, "RawId mismatch (want bio1)")
			},
		},
		{
			name:  "multiple devices, uses pin",
			fido2: newFakeFIDO2(auth1, pin1, pin2, bio1),
			setUP: pin2.setUP,
			createCredential: func() *wanlib.CredentialCreation {
				cp := *baseCC
				return &cp
			},
			prompt: pin2,
			assertResponse: func(t *testing.T, ccr *wanpb.CredentialCreationResponse, attObj *protocol.AttestationObject) {
				assert.Equal(t, pin2.credentialID(), ccr.RawId, "RawId mismatch (want pin2)")
			},
		},
		{
			name:  "excluded devices, single valid",
			fido2: newFakeFIDO2(auth1, bio1),
			setUP: bio1.setUP,
			createCredential: func() *wanlib.CredentialCreation {
				cp := *baseCC
				cp.Response.CredentialExcludeList = []protocol.CredentialDescriptor{
					{
						Type:         protocol.PublicKeyCredentialType,
						CredentialID: auth1.credentialID(),
					},
				}
				return &cp
			},
			assertResponse: func(t *testing.T, ccr *wanpb.CredentialCreationResponse, attObj *protocol.AttestationObject) {
				assert.Equal(t, bio1.credentialID(), ccr.RawId, "RawId mismatch (want bio1)")
			},
		},
		{
			name:  "excluded devices, multiple valid",
			fido2: newFakeFIDO2(auth1, pin1, pin2, bio1),
			setUP: bio1.setUP,
			createCredential: func() *wanlib.CredentialCreation {
				cp := *baseCC
				cp.Response.CredentialExcludeList = []protocol.CredentialDescriptor{
					{
						Type:         protocol.PublicKeyCredentialType,
						CredentialID: pin1.credentialID(),
					},
					{
						Type:         protocol.PublicKeyCredentialType,
						CredentialID: pin2.credentialID(),
					},
				}
				return &cp
			},
			assertResponse: func(t *testing.T, ccr *wanpb.CredentialCreationResponse, attObj *protocol.AttestationObject) {
				assert.Equal(t, bio1.credentialID(), ccr.RawId, "RawId mismatch (want bio1)")
			},
		},
		{
			name:    "NOK timeout without devices",
			timeout: 10 * time.Millisecond,
			fido2:   newFakeFIDO2(),
			setUP:   func() {},
			createCredential: func() *wanlib.CredentialCreation {
				cp := *baseCC
				return &cp
			},
			wantErr: context.DeadlineExceeded,
		},
		{
			name:  "passwordless pin device",
			fido2: newFakeFIDO2(pin2),
			setUP: pin2.setUP,
			createCredential: func() *wanlib.CredentialCreation {
				cp := pwdlessCC
				return &cp
			},
			prompt: pin2,
			assertResponse: func(t *testing.T, ccr *wanpb.CredentialCreationResponse, attObj *protocol.AttestationObject) {
				require.NotEmpty(t, pin2.credentials, "no resident credentials added to pin2")
				cred := pin2.credentials[len(pin2.credentials)-1]
				assert.Equal(t, cred.ID, ccr.RawId, "RawId mismatch (want pin2 resident credential)")
			},
		},
		{
			name:  "passwordless bio device",
			fido2: newFakeFIDO2(bio1),
			setUP: bio1.setUP,
			createCredential: func() *wanlib.CredentialCreation {
				cp := pwdlessCC
				return &cp
			},
			prompt: bio1,
			assertResponse: func(t *testing.T, ccr *wanpb.CredentialCreationResponse, attObj *protocol.AttestationObject) {
				require.NotEmpty(t, bio1.credentials, "no resident credentials added to bio1")
				cred := bio1.credentials[len(bio1.credentials)-1]
				assert.Equal(t, cred.ID, ccr.RawId, "RawId mismatch (want bio1 resident credential)")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.fido2.setCallbacks()
			test.setUP()

			timeout := test.timeout
			if timeout == 0 {
				timeout = 1 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			prompt := test.prompt
			if prompt == nil {
				prompt = noopPrompt{}
			}
			mfaResp, err := wancli.FIDO2Register(ctx, origin, test.createCredential(), prompt)
			switch {
			case test.wantErr != nil && err == nil:
				t.Fatalf("FIDO2Register returned err = nil, wantErr %q", test.wantErr)
			case test.wantErr != nil:
				require.True(t, errors.Is(err, test.wantErr), "FIDO2Register returned err = %q, wantErr %q", err, test.wantErr)
				return
			default:
				require.NoError(t, err, "FIDO2Register failed")
				require.NotNil(t, mfaResp, "mfaResp nil")
			}

			// Do a few baseline checks, tests can assert further.
			got := mfaResp.GetWebauthn()
			require.NotNil(t, got, "credential response nil")
			require.NotNil(t, got.Response, "attestation response nil")
			assert.NotNil(t, got.Response.ClientDataJson, "ClientDataJSON nil")
			want := &wanpb.CredentialCreationResponse{
				Type:  string(protocol.PublicKeyCredentialType),
				RawId: got.RawId,
				Response: &wanpb.AuthenticatorAttestationResponse{
					ClientDataJson:    got.Response.ClientDataJson,
					AttestationObject: got.Response.AttestationObject,
				},
				Extensions: got.Extensions,
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("FIDO2Register()/CredentialCreationResponse mismatch (-want +got):\n%v", diff)
			}

			attObj := &protocol.AttestationObject{}
			err = cbor.Unmarshal(got.Response.AttestationObject, attObj)
			require.NoError(t, err, "Failed to unmarshal AttestationObject")
			assert.Equal(t, makeCredentialAuthDataRaw, attObj.RawAuthData, "RawAuthData mismatch")

			if test.assertResponse != nil {
				test.assertResponse(t, got, attObj)
			}
		})
	}
}

func TestFIDO2Register_errors(t *testing.T) {
	resetFIDO2AfterTests(t)

	// Make sure we won't call the real libfido2.
	f2 := newFakeFIDO2()
	f2.setCallbacks()

	const origin = "https://example.com"
	okCC := &wanlib.CredentialCreation{
		Response: protocol.PublicKeyCredentialCreationOptions{
			Challenge: make([]byte, 32),
			RelyingParty: protocol.RelyingPartyEntity{
				ID: "example.com",
			},
			Parameters: []protocol.CredentialParameter{
				{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgES256},
			},
			AuthenticatorSelection: protocol.AuthenticatorSelection{
				UserVerification: protocol.VerificationDiscouraged,
			},
			Attestation: protocol.PreferNoAttestation,
		},
	}

	pwdlessOK := *okCC
	pwdlessOK.Response.RelyingParty.Name = "Teleport"
	pwdlessOK.Response.User = protocol.UserEntity{
		CredentialEntity: protocol.CredentialEntity{
			Name: "llama",
		},
		DisplayName: "Llama",
		ID:          []byte{1, 2, 3, 4, 5}, // arbitrary
	}
	rrk := true
	pwdlessOK.Response.AuthenticatorSelection.RequireResidentKey = &rrk
	pwdlessOK.Response.AuthenticatorSelection.UserVerification = protocol.VerificationRequired

	var prompt noopPrompt

	tests := []struct {
		name     string
		origin   string
		createCC func() *wanlib.CredentialCreation
		prompt   wancli.RegisterPrompt
		wantErr  string
	}{
		{
			name:     "ok - timeout", // check that good params are good
			origin:   origin,
			createCC: func() *wanlib.CredentialCreation { return okCC },
			prompt:   prompt,
			wantErr:  context.DeadlineExceeded.Error(),
		},
		{
			name:     "nil origin",
			createCC: func() *wanlib.CredentialCreation { return okCC },
			prompt:   prompt,
			wantErr:  "origin",
		},
		{
			name:     "nil cc",
			origin:   origin,
			createCC: func() *wanlib.CredentialCreation { return nil },
			prompt:   prompt,
			wantErr:  "credential creation required",
		},
		{
			name:   "cc without challenge",
			origin: origin,
			createCC: func() *wanlib.CredentialCreation {
				cp := *okCC
				cp.Response.Challenge = nil
				return &cp
			},
			prompt:  prompt,
			wantErr: "challenge",
		},
		{
			name:   "cc without RPID",
			origin: origin,
			createCC: func() *wanlib.CredentialCreation {
				cp := *okCC
				cp.Response.RelyingParty.ID = ""
				return &cp
			},
			prompt:  prompt,
			wantErr: "relying party ID",
		},
		{
			name:   "cc unsupported parameters",
			origin: origin,
			createCC: func() *wanlib.CredentialCreation {
				cp := *okCC
				cp.Response.Parameters = []protocol.CredentialParameter{
					{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgEdDSA},
				}
				return &cp
			},
			prompt:  prompt,
			wantErr: "ES256",
		},
		{
			name:     "nil pinPrompt",
			origin:   origin,
			createCC: func() *wanlib.CredentialCreation { return okCC },
			wantErr:  "prompt",
		},
		{
			name:   "rrk empty RP name",
			origin: origin,
			createCC: func() *wanlib.CredentialCreation {
				cp := pwdlessOK
				cp.Response.RelyingParty.Name = ""
				return &cp
			},
			prompt:  prompt,
			wantErr: "relying party name",
		},
		{
			name:   "rrk empty user name",
			origin: origin,
			createCC: func() *wanlib.CredentialCreation {
				cp := pwdlessOK
				cp.Response.User.Name = ""
				return &cp
			},
			prompt:  prompt,
			wantErr: "user name",
		},
		{
			name:   "rrk empty user display name",
			origin: origin,
			createCC: func() *wanlib.CredentialCreation {
				cp := pwdlessOK
				cp.Response.User.DisplayName = ""
				return &cp
			},
			prompt:  prompt,
			wantErr: "user display name",
		},
		{
			name:   "rrk nil user ID",
			origin: origin,
			createCC: func() *wanlib.CredentialCreation {
				cp := pwdlessOK
				cp.Response.User.ID = nil
				return &cp
			},
			prompt:  prompt,
			wantErr: "user ID",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
			defer cancel()

			_, err := wancli.FIDO2Register(ctx, test.origin, test.createCC(), test.prompt)
			require.Error(t, err, "FIDO2Register returned err = nil, want %q", test.wantErr)
			assert.Contains(t, err.Error(), test.wantErr, "FIDO2Register returned err = %q, want %q", err, test.wantErr)
		})
	}
}

func resetFIDO2AfterTests(t *testing.T) {
	pollInterval := wancli.FIDO2PollInterval
	devLocations := wancli.FIDODeviceLocations
	newDevice := wancli.FIDONewDevice
	t.Cleanup(func() {
		wancli.FIDO2PollInterval = pollInterval
		wancli.FIDODeviceLocations = devLocations
		wancli.FIDONewDevice = newDevice
	})
}

type fakeFIDO2 struct {
	useNonMeteredLocs bool

	locs    []*libfido2.DeviceLocation
	devices map[string]*fakeFIDO2Device
}

func newFakeFIDO2(devs ...*fakeFIDO2Device) *fakeFIDO2 {
	f := &fakeFIDO2{
		devices: make(map[string]*fakeFIDO2Device),
	}
	for _, dev := range devs {
		if _, ok := f.devices[dev.path]; ok {
			panic(fmt.Sprintf("Duplicate device path registered: %q", dev.path))
		}
		f.locs = append(f.locs, &libfido2.DeviceLocation{
			Path: dev.path,
		})
		f.devices[dev.path] = dev
	}
	return f
}

// withNonMeteredLocations makes fakeFIDO2 return all known devices immediately.
// Useful to test flows that optimize for plugged devices.
func (f *fakeFIDO2) withNonMeteredLocations() *fakeFIDO2 {
	f.useNonMeteredLocs = true
	return f
}

func (f *fakeFIDO2) setCallbacks() {
	if f.useNonMeteredLocs {
		*wancli.FIDODeviceLocations = f.DeviceLocations
	} else {
		*wancli.FIDODeviceLocations = f.newMeteredDeviceLocations()
	}
	*wancli.FIDONewDevice = f.NewDevice
}

func (f *fakeFIDO2) newMeteredDeviceLocations() func() ([]*libfido2.DeviceLocation, error) {
	i := 0
	return func() ([]*libfido2.DeviceLocation, error) {
		// Delay showing devices for a while to exercise polling.
		i++
		const minLoops = 2
		if i < minLoops {
			return nil, nil
		}
		return f.locs, nil
	}
}

func (f *fakeFIDO2) DeviceLocations() ([]*libfido2.DeviceLocation, error) {
	return f.locs, nil
}

func (f *fakeFIDO2) NewDevice(path string) (wancli.FIDODevice, error) {
	if dev, ok := f.devices[path]; ok {
		return dev, nil
	}
	// go-libfido2 doesn't actually error here, but we do for simplicity.
	return nil, errors.New("not found")
}

type fakeFIDO2Device struct {
	simplePicker

	// Set to true to cause "unsupported option" UV errors, regardless of other
	// conditions.
	failUV bool

	// Set to true to simulate an U2F-only device.
	// Causes libfido2.ErrNotFIDO2 on Info.
	u2fOnly bool

	// assertionErrors is a chain of errors to return from Assertion.
	// Errors are returned from start to end and removed, one-by-one, on each
	// invocation of the Assertion method.
	// If the slice is empty, Assertion runs normally.
	assertionErrors []error

	path        string
	info        *libfido2.DeviceInfo
	pin         string
	credentials []*libfido2.Credential

	// wantRPID may be set directly to enable RPID checks on Assertion.
	wantRPID string
	// format may be set directly to change the attestation format.
	format string

	key    *mocku2f.Key
	pubKey []byte

	// cond guards up and cancel.
	cond       *sync.Cond
	up, cancel bool
}

func mustNewFIDO2Device(path, pin string, info *libfido2.DeviceInfo, creds ...*libfido2.Credential) *fakeFIDO2Device {
	dev, err := newFIDO2Device(path, pin, info, creds...)
	if err != nil {
		panic(err)
	}
	return dev
}

func newFIDO2Device(path, pin string, info *libfido2.DeviceInfo, creds ...*libfido2.Credential) (*fakeFIDO2Device, error) {
	key, err := mocku2f.Create()
	if err != nil {
		return nil, err
	}

	pubKeyCBOR, err := wanlib.U2FKeyToCBOR(&key.PrivateKey.PublicKey)
	if err != nil {
		return nil, err
	}

	for _, cred := range creds {
		cred.ID = make([]byte, 16) // somewhat arbitrary
		if _, err := rand.Read(cred.ID); err != nil {
			return nil, err
		}
		cred.Type = libfido2.ES256
	}

	return &fakeFIDO2Device{
		path:        path,
		pin:         pin,
		credentials: creds,
		format:      "packed",
		info:        info,
		key:         key,
		pubKey:      pubKeyCBOR,
		cond:        sync.NewCond(&sync.Mutex{}),
	}, nil
}

func (f *fakeFIDO2Device) PromptPIN() (string, error) {
	return f.pin, nil
}

func (f *fakeFIDO2Device) PromptTouch() error {
	f.setUP()
	return nil
}

func (f *fakeFIDO2Device) credentialID() []byte {
	return f.key.KeyHandle
}

func (f *fakeFIDO2Device) cert() []byte {
	return f.key.Cert
}

func (f *fakeFIDO2Device) Info() (*libfido2.DeviceInfo, error) {
	if f.u2fOnly {
		return nil, libfido2.ErrNotFIDO2
	}
	return f.info, nil
}

func (f *fakeFIDO2Device) setUP() {
	f.cond.L.Lock()
	f.up = true
	f.cond.L.Unlock()
	f.cond.Broadcast()
}

func (f *fakeFIDO2Device) Cancel() error {
	f.cond.L.Lock()
	f.cancel = true
	f.cond.L.Unlock()
	f.cond.Broadcast()
	return nil
}

func (f *fakeFIDO2Device) MakeCredential(
	clientDataHash []byte,
	rp libfido2.RelyingParty,
	user libfido2.User,
	typ libfido2.CredentialType,
	pin string,
	opts *libfido2.MakeCredentialOpts,
) (*libfido2.Attestation, error) {
	switch {
	case len(clientDataHash) == 0:
		return nil, errors.New("clientDataHash required")
	case rp.ID == "":
		return nil, errors.New("rp.ID required")
	case typ != libfido2.ES256:
		return nil, errors.New("bad credential type")
	case opts.UV == libfido2.False: // can only be empty or true
		return nil, libfido2.ErrUnsupportedOption
	case opts.UV == libfido2.True && !f.hasUV():
		return nil, libfido2.ErrUnsupportedOption // PIN authenticators don't like UV
	case opts.RK == libfido2.True && !f.hasRK():
		// TODO(codingllama): Confirm scenario with a real authenticator.
		return nil, libfido2.ErrUnsupportedOption
	}

	// Validate PIN regardless of opts.
	// This is in line with how current YubiKeys behave.
	if err := f.validatePIN(pin); err != nil {
		return nil, err
	}

	if err := f.maybeLockUntilInteraction(true /* up */); err != nil {
		return nil, err
	}

	cert, sig := f.cert(), makeCredentialSig
	if f.format == "none" {
		// Do not return attestation data in case of "none".
		// This is a hypothetical scenario, as I haven't seen device that does this.
		cert, sig = nil, nil
	}

	// Did we create a resident credential? Create a new ID for it and record it.
	cID := f.key.KeyHandle
	if opts.RK == libfido2.True {
		cID = make([]byte, 16) // somewhat arbitrary
		if _, err := rand.Read(cID); err != nil {
			return nil, err
		}
		f.credentials = append(f.credentials, &libfido2.Credential{
			ID:   cID,
			Type: libfido2.ES256,
			User: user,
		})
	}

	return &libfido2.Attestation{
		ClientDataHash: clientDataHash,
		AuthData:       makeCredentialAuthDataCBOR,
		CredentialID:   cID,
		CredentialType: libfido2.ES256,
		PubKey:         f.pubKey,
		Cert:           cert,
		Sig:            sig,
		Format:         f.format,
	}, nil
}

func (f *fakeFIDO2Device) Assertion(
	rpID string,
	clientDataHash []byte,
	credentialIDs [][]byte,
	pin string,
	opts *libfido2.AssertionOpts,
) ([]*libfido2.Assertion, error) {
	// Give preference to simulated errors.
	if len(f.assertionErrors) > 0 {
		err := f.assertionErrors[0]
		f.assertionErrors = f.assertionErrors[1:]
		return nil, err
	}

	switch {
	case rpID == "":
		return nil, errors.New("rp.ID required")
	case f.wantRPID != "" && f.wantRPID != rpID:
		return nil, libfido2.ErrNoCredentials
	case len(clientDataHash) == 0:
		return nil, errors.New("clientDataHash required")
	}

	// Validate UV.
	switch {
	case opts.UV == "": // OK, actually works as false.
	case opts.UV == libfido2.True && f.failUV:
		// Emulate UV failures, as seen in some devices regardless of other
		// settings.
		return nil, libfido2.ErrUnsupportedOption
	case opts.UV == libfido2.True && f.isBio(): // OK.
	case opts.UV == libfido2.True && f.hasClientPin() && pin != "": // OK, doubles as UV.
	default: // Anything else is invalid, including libfido2.False.
		return nil, libfido2.ErrUnsupportedOption
	}

	// Validate PIN only if present and UP is required.
	// This is in line with how current YubiKeys behave.
	// TODO(codingllama): This should probably take UV into consideration.
	privilegedAccess := f.isBio()
	if pin != "" && opts.UP == libfido2.True {
		if err := f.validatePIN(pin); err != nil {
			return nil, err
		}
		privilegedAccess = true
	}

	// Block for user presence before accessing any credential data.
	if err := f.maybeLockUntilInteraction(opts.UP == libfido2.True); err != nil {
		return nil, err
	}

	// Index credentialIDs for easier use.
	credIDs := make(map[string]struct{})
	for _, cred := range credentialIDs {
		credIDs[string(cred)] = struct{}{}
	}

	// Assemble one assertion for each allowed credential we hold.
	var assertions []*libfido2.Assertion

	// "base" credential. Only add an assertion if explicitly requested.
	if _, ok := credIDs[string(f.key.KeyHandle)]; ok {
		assertions = append(assertions, &libfido2.Assertion{
			AuthDataCBOR: assertionAuthDataCBOR,
			Sig:          assertionSig,
			CredentialID: f.key.KeyHandle,
			User:         libfido2.User{
				// We don't hold data about the user for the "base" credential / MFA
				// scenario.
				// A typical authenticator might choose to save some data within the
				// key handle itself.
			},
		})
	}

	// Resident credentials.
	if privilegedAccess {
		for _, resident := range f.credentials {
			allowed := len(credIDs) == 0
			if !allowed {
				_, allowed = credIDs[string(resident.ID)]
			}
			if !allowed {
				continue
			}
			assertions = append(assertions, &libfido2.Assertion{
				AuthDataCBOR: assertionAuthDataCBOR,
				Sig:          assertionSig,
				HMACSecret:   []byte{},
				CredentialID: resident.ID,
				User: libfido2.User{
					ID:          resident.User.ID,
					Name:        resident.User.Name,
					DisplayName: resident.User.DisplayName,
					Icon:        resident.User.Icon,
				},
			})
		}
	}

	switch len(assertions) {
	case 0:
		return nil, libfido2.ErrNoCredentials
	case 1:
		// Remove user name / display name / icon.
		// See the authenticatorGetAssertion response structure, user member (0x04):
		// https://fidoalliance.org/specs/fido-v2.1-ps-20210615/fido-client-to-authenticator-protocol-v2.1-ps-20210615.html#authenticatorgetassertion-response-structure
		assertions[0].User.Name = ""
		assertions[0].User.DisplayName = ""
		assertions[0].User.Icon = ""
		return assertions, nil
	default:
		return assertions, nil
	}
}

func (f *fakeFIDO2Device) validatePIN(pin string) error {
	switch {
	case f.isBio() && pin == "": // OK, biometric check supersedes PIN.
	case f.pin != "" && pin == "":
		return libfido2.ErrPinRequired
	case f.pin != "" && f.pin != pin:
		return libfido2.ErrPinInvalid
	}
	return nil
}

func (f *fakeFIDO2Device) hasClientPin() bool {
	return f.hasBoolOpt("clientPin")
}

func (f *fakeFIDO2Device) hasRK() bool {
	return f.hasBoolOpt("rk")
}

func (f *fakeFIDO2Device) hasUV() bool {
	return f.hasBoolOpt("uv")
}

func (f *fakeFIDO2Device) isBio() bool {
	return f.hasBoolOpt("bioEnroll")
}

func (f *fakeFIDO2Device) hasBoolOpt(name string) bool {
	if f.info == nil {
		return false
	}

	for _, opt := range f.info.Options {
		if opt.Name == name {
			return opt.Value == libfido2.True
		}
	}
	return false
}

func (f *fakeFIDO2Device) maybeLockUntilInteraction(up bool) error {
	if !up {
		return nil // without UserPresence it doesn't lock.
	}

	// Lock until we get a touch or a cancel.
	f.cond.L.Lock()
	for !f.up && !f.cancel {
		f.cond.Wait()
	}
	defer f.cond.L.Unlock()

	// Record/reset state.
	isCancel := f.cancel
	f.up = false
	f.cancel = false

	if isCancel {
		return libfido2.ErrKeepaliveCancel
	}
	return nil
}
