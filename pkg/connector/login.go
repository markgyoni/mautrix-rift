package connector

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/markgyoni/mautrix-rift/pkg/riftclient"
)

const (
	LoginFlowDevice  = "device"
	LoginFlowRiftKey = "rift-key"

	stepCredentials = "chat.rift.login.credentials"
	stepTOTP        = "chat.rift.login.totp"
	stepPairing     = "chat.rift.login.pairing"
	stepComplete    = "chat.rift.login.complete"

	pairingDeviceLabel = "Matrix bridge (mautrix-rift)"
	pairingPollEvery   = 2500 * time.Millisecond
)

func (rc *RiftConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{{
		Name:        "Approve from another device",
		Description: "Log in with your username and password, then approve the bridge from the Rift app with a 9-digit code",
		ID:          LoginFlowDevice,
	}, {
		Name:        "Rift Key",
		Description: "Log in with your username, password and 12-word Rift Key",
		ID:          LoginFlowRiftKey,
	}}
}

func (rc *RiftConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	if flowID != LoginFlowDevice && flowID != LoginFlowRiftKey {
		return nil, fmt.Errorf("unknown login flow %q", flowID)
	}
	return &RiftLogin{main: rc, user: user, flow: flowID, client: riftclient.NewClient("", "")}, nil
}

// RiftLogin handles both flows. Rift Key: credentials (+ Rift Key) → optional TOTP → done.
// Device approval: credentials → show pairing code and wait → done.
type RiftLogin struct {
	main   *RiftConnector
	user   *bridgev2.User
	flow   string
	client *riftclient.Client

	username, password string
	keys               *riftclient.AccountKeys // Rift Key flow
	loginTicket        string                  // pending 2FA login

	pairing *riftclient.PairingRequest
	session *riftclient.TrustedLoginSession
}

var (
	_ bridgev2.LoginProcessUserInput      = (*RiftLogin)(nil)
	_ bridgev2.LoginProcessDisplayAndWait = (*RiftLogin)(nil)
)

func (l *RiftLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	fields := []bridgev2.LoginInputDataField{
		{Type: bridgev2.LoginInputFieldTypeUsername, ID: "username", Name: "Username"},
		{Type: bridgev2.LoginInputFieldTypePassword, ID: "password", Name: "Password"},
	}
	instructions := "Enter your Rift username and password. You'll then get a 9-digit code to approve the bridge from a device where you're signed in to Rift."
	if l.flow == LoginFlowRiftKey {
		instructions = "Enter your Rift credentials and Rift Key. The Rift Key is needed to read and send encrypted DMs."
		fields = append(fields, bridgev2.LoginInputDataField{
			Type: bridgev2.LoginInputFieldTypePassword, ID: "rift_key", Name: "Rift Key",
			Description: "Your 12-word recovery phrase, separated by spaces",
			Validate: func(s string) (string, error) {
				s = strings.Join(strings.Fields(strings.ToLower(s)), " ")
				if n := len(strings.Fields(s)); n != 12 {
					return "", fmt.Errorf("expected 12 words, got %d", n)
				}
				return s, nil
			},
		})
	}
	return &bridgev2.LoginStep{
		Type:            bridgev2.LoginStepTypeUserInput,
		StepID:          stepCredentials,
		Instructions:    instructions,
		UserInputParams: &bridgev2.LoginUserInputParams{Fields: fields},
	}, nil
}

func (l *RiftLogin) Cancel() {
	if l.session != nil {
		_ = l.client.CancelTrustedLogin(context.Background(), l.session)
		l.session = nil
	}
}

func totpStep() *bridgev2.LoginStep {
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       stepTOTP,
		Instructions: "Enter the 6-digit code from your authenticator app.",
		UserInputParams: &bridgev2.LoginUserInputParams{Fields: []bridgev2.LoginInputDataField{{
			Type: bridgev2.LoginInputFieldType2FACode, ID: "totp_code", Name: "Two-factor code",
			Validate: func(s string) (string, error) {
				s = strings.ReplaceAll(strings.TrimSpace(s), " ", "")
				if len(s) != 6 || strings.Trim(s, "0123456789") != "" {
					return "", errors.New("expected 6 digits")
				}
				return s, nil
			},
		}}},
	}
}

func (l *RiftLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	if l.loginTicket != "" {
		return l.submitTOTP(ctx, input["totp_code"])
	}
	l.username, l.password = strings.TrimSpace(input["username"]), input["password"]
	if l.flow == LoginFlowDevice {
		return l.startPairing(ctx)
	}

	keys, err := riftclient.DeriveKeysFromMnemonic(input["rift_key"])
	if err != nil {
		return nil, bridgev2.RespError{ErrCode: "FI.MAU.RIFT_INVALID_KEY", Err: err.Error(), StatusCode: 400}
	}
	l.keys = keys
	auth, err := l.client.Login(ctx, l.username, l.password)
	if ticket := riftclient.TOTPLoginTicket(err); ticket != "" {
		l.loginTicket = ticket
		return totpStep(), nil
	} else if err != nil {
		return nil, fmt.Errorf("login failed: %w", err)
	}
	return l.finish(ctx, auth.User, keys)
}

func (l *RiftLogin) submitTOTP(ctx context.Context, code string) (*bridgev2.LoginStep, error) {
	auth, err := l.client.TOTPLogin(ctx, l.loginTicket, code)
	switch riftclient.ErrorCode(err) {
	case "":
	case riftclient.ErrCodeInvalidTOTP:
		return nil, bridgev2.RespError{ErrCode: "FI.MAU.RIFT_INVALID_TOTP", Err: "Wrong two-factor code, try again", StatusCode: 400}
	case riftclient.ErrCodeTOTPTicketExpired:
		return nil, bridgev2.RespError{ErrCode: "FI.MAU.RIFT_TOTP_EXPIRED", Err: "The login expired, please start over", StatusCode: 400}
	}
	if err != nil {
		return nil, fmt.Errorf("two-factor login failed: %w", err)
	}
	l.loginTicket = ""
	return l.finish(ctx, auth.User, l.keys)
}

func (l *RiftLogin) startPairing(ctx context.Context) (*bridgev2.LoginStep, error) {
	req, err := riftclient.NewPairingRequest()
	if err != nil {
		return nil, err
	}
	sess, err := l.client.InitiateTrustedLogin(ctx, l.username, l.password, pairingDeviceLabel, req)
	if err != nil {
		// Confirmed 2026-11-23: no TOTP step here even with 2FA on; approval stands in for it.
		return nil, fmt.Errorf("login failed: %w", err)
	}
	l.pairing, l.session = req, sess
	code := req.Code[:3] + " " + req.Code[3:6] + " " + req.Code[6:]
	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeDisplayAndWait,
		StepID: stepPairing,
		Instructions: "Open Rift on a device where you're signed in and approve the new device with this code. " +
			"It expires in 10 minutes.",
		DisplayAndWaitParams: &bridgev2.LoginDisplayAndWaitParams{Type: bridgev2.LoginDisplayTypeCode, Data: code, CanCancel: true},
	}, nil
}

func (l *RiftLogin) Wait(ctx context.Context) (*bridgev2.LoginStep, error) {
	if l.session == nil {
		return nil, errors.New("no pairing in progress")
	}
	failures := 0
	for {
		select {
		case <-ctx.Done():
			l.Cancel()
			if errors.Is(context.Cause(ctx), bridgev2.ErrLoginStepCancelled) {
				return nil, bridgev2.ErrLoginStepCancelled
			}
			return nil, ctx.Err()
		case <-time.After(pairingPollEvery):
		}
		st, err := l.client.TrustedLoginStatus(ctx, l.session)
		if err != nil {
			var httpErr *riftclient.HTTPError
			if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403 || httpErr.Status == 404) {
				return nil, errors.New("the pairing request is no longer valid, please start over")
			}
			if failures++; failures >= 5 {
				return nil, fmt.Errorf("checking pairing status: %w", err)
			}
			continue
		}
		failures = 0
		switch st.Status {
		case "pending":
			continue
		case "denied":
			l.session = nil
			return nil, bridgev2.RespError{ErrCode: "FI.MAU.RIFT_PAIRING_DENIED", Err: "The login was denied on your other device", StatusCode: 403}
		case "expired":
			l.session = nil
			return nil, bridgev2.RespError{ErrCode: "FI.MAU.RIFT_PAIRING_EXPIRED", Err: "The code expired, please start over", StatusCode: 400}
		case "approved":
			sessionID := l.session.SessionID
			l.session = nil
			return l.finishPairing(ctx, sessionID, st)
		default:
			return nil, fmt.Errorf("unexpected pairing status %q", st.Status)
		}
	}
}

func (l *RiftLogin) finishPairing(ctx context.Context, sessionID string, st *riftclient.TrustedLoginStatus) (*bridgev2.LoginStep, error) {
	if st.EncryptedBundle == "" || st.AccessToken == "" || st.RefreshToken == "" || st.User == nil {
		return nil, errors.New("the approval was incomplete, please start over")
	}
	l.client.SetTokens(st.AccessToken, st.RefreshToken)
	fail := func(err error) (*bridgev2.LoginStep, error) {
		_ = l.client.Logout(ctx)
		return nil, err
	}
	ct, err1 := hex.DecodeString(st.EncryptedBundle)
	iv, err2 := hex.DecodeString(st.BundleIV)
	approver, err3 := hex.DecodeString(st.ApproverPubkey)
	if err := errors.Join(err1, err2, err3); err != nil {
		return fail(fmt.Errorf("malformed pairing response: %w", err))
	}
	pt, err := l.pairing.DecryptBundle(sessionID, approver, iv, ct)
	if errors.Is(err, riftclient.ErrPairingCodeMismatch) {
		return fail(bridgev2.RespError{ErrCode: "FI.MAU.RIFT_PAIRING_MISMATCH", Err: "The code didn't match, please start over", StatusCode: 400})
	} else if err != nil {
		return fail(err)
	}
	bundle, err := riftclient.ParsePairingBundle(pt)
	if err != nil {
		return fail(err)
	}
	keys, err := bundle.AccountKeys()
	if err != nil {
		return fail(err)
	}
	return l.finish(ctx, st.User, keys)
}

// finish checks the keys against the server and saves the login.
func (l *RiftLogin) finish(ctx context.Context, user *riftclient.User, keys *riftclient.AccountKeys) (*bridgev2.LoginStep, error) {
	ident, err := l.client.GetPeerIdentity(ctx, user.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch account keys: %w", err)
	}
	if !strings.EqualFold(ident.IdentityKey, keys.DMPublicHex()) ||
		(ident.AccountSigningPublicKey != "" && !strings.EqualFold(ident.AccountSigningPublicKey, keys.SigningPublicHex())) {
		_ = l.client.Logout(ctx)
		return nil, bridgev2.RespError{ErrCode: "FI.MAU.RIFT_WRONG_KEY", Err: "That Rift Key doesn't belong to this account", StatusCode: 400}
	}
	if sig, err := hex.DecodeString(ident.IdentityKeySignature); err != nil || !ed25519.Verify(keys.SigningPub, keys.DMPublic, sig) {
		// Not fatal; the format of the signed message may differ. Logged for investigation.
		l.user.Log.Warn().Msg("Own identity key signature didn't verify with derived account key")
	}

	access, refresh := l.client.Tokens()
	ul, err := l.user.NewLogin(ctx, &database.UserLogin{
		ID:         networkid.UserLoginID(user.ID),
		RemoteName: user.Username,
		RemoteProfile: status.RemoteProfile{
			Username: user.Username,
			Name:     user.Name(),
		},
		Metadata: &UserLoginMetadata{
			AccessToken:  access,
			RefreshToken: refresh,
			AccountSeed:  hex.EncodeToString(keys.SigningSeed),
		},
	}, &bridgev2.NewLoginParams{DeleteOnConflict: true})
	if err != nil {
		return nil, err
	}
	ul.Client.Connect(ul.Log.WithContext(context.Background()))
	return &bridgev2.LoginStep{
		Type:           bridgev2.LoginStepTypeComplete,
		StepID:         stepComplete,
		Instructions:   fmt.Sprintf("Logged in as %s", user.Username),
		CompleteParams: &bridgev2.LoginCompleteParams{UserLoginID: ul.ID, UserLogin: ul},
	}, nil
}
