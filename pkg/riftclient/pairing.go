package riftclient

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gtank/ristretto255"
)

// Device pairing ("trusted login"): the new device shows a 9-digit code, the user types it on a
// signed-in device, and the two run CPace (draft-irtf-cfrg-cpace, ristretto255 + SHA-512) keyed by
// that code. The approver seals the DM vault under the CPace session key and the server relays it.
// Parameters and test vector: therift.chat/encryption ("Adding a device").
const (
	cpaceDSI          = "CPaceRistretto255"
	cpaceDSIISK       = "CPaceRistretto255_ISK"
	cpaceSInBytes     = 128 // SHA-512 block size
	pairingChannelID  = "rift-device-pairing-v2"
	pairingADNew      = "new-device"
	pairingADApprover = "approver"
	pairingBundleInfo = "rift-pairing-bundle-v2"
	PairingCodeLength = 9
)

var ErrPairingCodeMismatch = errors.New("rift: pairing failed (code mismatch or tampered bundle)")

// lv prepends the LEB128 length, as in the CPace draft.
func lv(b []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(len(b)))
	return append(out, b...)
}

func lvCat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, lv(p)...)
	}
	return out
}

func cpaceGenerator(password string) *ristretto255.Element {
	zpad := max(0, cpaceSInBytes-1-len(lv([]byte(password)))-len(lv([]byte(cpaceDSI))))
	h := sha512.Sum512(lvCat([]byte(cpaceDSI), []byte(password), make([]byte, zpad), []byte(pairingChannelID), nil))
	return ristretto255.NewElement().FromUniformBytes(h[:])
}

// PairingRequest is the new device's half of a CPace exchange.
type PairingRequest struct {
	Code   string // 9 digits, shown to the user
	secret *ristretto255.Scalar
	Share  []byte // ephemeral_pubkey sent to /pairing/trusted-login/initiate
}

// NewPairingRequest generates a pairing code and CPace share.
func NewPairingRequest() (*PairingRequest, error) {
	code, err := generatePairingCode()
	if err != nil {
		return nil, err
	}
	for {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		b[31] &= 0x0f // < 2^252, so always below the group order
		if req, err := newPairingRequest(code, b[:]); err == nil {
			return req, nil
		}
	}
}

func newPairingRequest(code string, scalar []byte) (*PairingRequest, error) {
	s, err := ristretto255.NewScalar().SetCanonicalBytes(scalar)
	if err != nil {
		return nil, err
	}
	if s.Equal(ristretto255.NewScalar()) == 1 {
		return nil, errors.New("zero scalar")
	}
	share := ristretto255.NewElement().ScalarMult(s, cpaceGenerator(code)).Bytes()
	return &PairingRequest{Code: code, secret: s, Share: share}, nil
}

// generatePairingCode returns 8 random digits plus a Luhn check digit, like the web client.
func generatePairingCode() (string, error) {
	digits := make([]byte, 0, PairingCodeLength)
	buf := make([]byte, 16)
	for len(digits) < PairingCodeLength-1 {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if b < 250 && len(digits) < PairingCodeLength-1 {
				digits = append(digits, '0'+b%10)
			}
		}
	}
	return string(digits) + string('0'+luhnCheckDigit(string(digits))), nil
}

func luhnCheckDigit(s string) byte {
	sum, double := 0, true
	for i := len(s) - 1; i >= 0; i-- {
		d := int(s[i] - '0')
		if double {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return byte((10 - sum%10) % 10)
}

// sessionKey computes the CPace ISK from the approver's share.
func (r *PairingRequest) sessionKey(approverShare []byte) ([]byte, error) {
	y, err := ristretto255.NewElement().SetCanonicalBytes(approverShare)
	if err != nil {
		return nil, ErrPairingCodeMismatch
	}
	k := ristretto255.NewElement().ScalarMult(r.secret, y)
	if k.Equal(ristretto255.NewIdentityElement()) == 1 {
		return nil, ErrPairingCodeMismatch
	}
	isk := sha512.Sum512(append(append(
		lvCat([]byte(cpaceDSIISK), nil, k.Bytes()),
		lvCat(r.Share, []byte(pairingADNew))...),
		lvCat(approverShare, []byte(pairingADApprover))...))
	return isk[:], nil
}

// DecryptBundle opens the bundle the approving device sealed for this request.
func (r *PairingRequest) DecryptBundle(sessionID string, approverShare, iv, ciphertext []byte) ([]byte, error) {
	isk, err := r.sessionKey(approverShare)
	if err != nil {
		return nil, err
	}
	key, err := hkdf.Key(sha256.New, isk, nil, pairingBundleInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(iv) != gcm.NonceSize() {
		return nil, fmt.Errorf("rift: pairing iv must be %d bytes", gcm.NonceSize())
	}
	pt, err := gcm.Open(nil, iv, ciphertext, []byte(pairingBundleInfo+"|"+strings.ToLower(sessionID)))
	if err != nil {
		return nil, ErrPairingCodeMismatch
	}
	return pt, nil
}

// PairingBundle is the DM vault export inside the pairing bundle (web client, not captured).
// signingPrivateKeyB64 is the account seed; everything else derives from it.
type PairingBundle struct {
	SigningPrivateKeyB64 string `json:"signingPrivateKeyB64"`
	DMPublicKeyHex       string `json:"dmPublicKeyHex"`
}

// AccountKeys derives the account keys from the bundle and checks them against its DM public key.
func (b *PairingBundle) AccountKeys() (*AccountKeys, error) {
	seed, err := base64.StdEncoding.DecodeString(b.SigningPrivateKeyB64)
	if err != nil {
		return nil, fmt.Errorf("rift: bad signing key in pairing bundle: %w", err)
	}
	keys, err := DeriveKeysFromSeed(seed)
	if err != nil {
		return nil, err
	}
	if b.DMPublicKeyHex != "" && !strings.EqualFold(b.DMPublicKeyHex, keys.DMPublicHex()) {
		return nil, errors.New("rift: pairing bundle DM key doesn't match its account key")
	}
	return keys, nil
}

func ParsePairingBundle(plaintext []byte) (*PairingBundle, error) {
	var b PairingBundle
	if err := json.Unmarshal(plaintext, &b); err != nil {
		return nil, fmt.Errorf("rift: decoding pairing bundle: %w", err)
	}
	if b.SigningPrivateKeyB64 == "" {
		return nil, errors.New("rift: pairing bundle has no account key")
	}
	return &b, nil
}
