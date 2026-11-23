package riftclient

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/tyler-smith/go-bip39"
	"golang.org/x/crypto/curve25519"
)

// Domain-separation labels, from therift.chat/encryption and the web client bundle.
const (
	labelIdentity = "rift-identity-v1"
	labelDMKey    = "rift-dm-e2ee-x25519-v1"
	labelSPKSalt  = "rift-dm-signed-prekey"
	labelSPKInfo  = "rift-dm-spk"
	labelOTPBase  = "rift-dm-otp-base"
)

// AccountKeys holds every key derived from a Rift Key (12-word BIP-39 mnemonic).
// Everything is derived from SigningSeed, so that is the only secret that needs persisting.
type AccountKeys struct {
	SigningSeed []byte // 32-byte Ed25519 seed (the "account private key")
	SigningPub  ed25519.PublicKey

	DMPrivate []byte // X25519 DM identity key
	DMPublic  []byte

	SignedPrekeyPrivate []byte // X25519, key_id 1; only used by the ratchet protocol
	SignedPrekeyPublic  []byte

	OTPBase []byte // base for one-time prekeys (derivation not yet reversed)
}

// DeriveKeysFromMnemonic turns a Rift Key into account keys.
// entropy = BIP-39 entropy; seed = SHA-256(entropy || "rift-identity-v1").
func DeriveKeysFromMnemonic(mnemonic string) (*AccountKeys, error) {
	words := strings.Join(strings.Fields(strings.ToLower(mnemonic)), " ")
	entropy, err := bip39.EntropyFromMnemonic(words)
	if err != nil {
		return nil, fmt.Errorf("invalid Rift Key: %w", err)
	}
	seed := sha256.Sum256(append(entropy, labelIdentity...))
	return DeriveKeysFromSeed(seed[:])
}

// DeriveKeysFromSeed derives the DM and prekey material from the Ed25519 account seed.
func DeriveKeysFromSeed(seed []byte) (*AccountKeys, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("account seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	k := &AccountKeys{SigningSeed: append([]byte(nil), seed...)}
	k.SigningPub = ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)

	dm := sha256.Sum256(append(append([]byte(nil), seed...), labelDMKey...))
	k.DMPrivate = dm[:]
	var err error
	if k.DMPublic, err = curve25519.X25519(k.DMPrivate, curve25519.Basepoint); err != nil {
		return nil, err
	}

	if k.SignedPrekeyPrivate, err = hkdf.Key(sha256.New, seed, []byte(labelSPKSalt), labelSPKInfo, 32); err != nil {
		return nil, err
	}
	if k.SignedPrekeyPublic, err = curve25519.X25519(k.SignedPrekeyPrivate, curve25519.Basepoint); err != nil {
		return nil, err
	}

	otp := sha256.Sum256(append(append([]byte(nil), seed...), labelOTPBase...))
	k.OTPBase = otp[:]
	return k, nil
}

// Sign signs msg with the Ed25519 account key.
func (k *AccountKeys) Sign(msg []byte) []byte {
	return ed25519.Sign(ed25519.NewKeyFromSeed(k.SigningSeed), msg)
}

func (k *AccountKeys) DMPublicHex() string           { return hex.EncodeToString(k.DMPublic) }
func (k *AccountKeys) SigningPublicHex() string      { return hex.EncodeToString(k.SigningPub) }
func (k *AccountKeys) SignedPrekeyPublicHex() string { return hex.EncodeToString(k.SignedPrekeyPublic) }
