package riftclient

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/curve25519"
)

const (
	DMv4Version   = 4
	DMv4Algorithm = "x25519-hkdf-sha256-aes-256-gcm"
	dmv4Label     = "rift-dm-v4"
)

// E2EEPayload is the e2ee_payload object on DM messages.
type E2EEPayload struct {
	Version            int    `json:"version"`
	Algorithm          string `json:"algorithm"`
	SenderPublicKey    string `json:"sender_public_key"`
	RecipientPublicKey string `json:"recipient_public_key"`
	Nonce              string `json:"nonce"`      // base64(salt16) "." base64(iv12)
	Ciphertext         string `json:"ciphertext"` // base64(ct || tag)
}

type dmv4AAD struct {
	V   int     `json:"v"`
	S   string  `json:"s"`
	R   string  `json:"r"`
	CID string  `json:"cid"`
	TID *string `json:"tid"`
}

func dmv4Key(myPriv []byte, senderHex, recipientHex string, salt []byte) ([]byte, error) {
	peerHex := recipientHex
	// The shared secret is symmetric; pick whichever key isn't ours.
	if myPub, err := curve25519.X25519(myPriv, curve25519.Basepoint); err == nil && strings.EqualFold(hex.EncodeToString(myPub), recipientHex) {
		peerHex = senderHex
	}
	peer, err := hex.DecodeString(peerHex)
	if err != nil || len(peer) != 32 {
		return nil, fmt.Errorf("invalid peer public key %q", peerHex)
	}
	shared, err := curve25519.X25519(myPriv, peer)
	if err != nil {
		return nil, err
	}
	info := dmv4Label + "|" + strings.ToLower(senderHex) + "|" + strings.ToLower(recipientHex)
	return hkdf.Key(sha256.New, shared, salt, info, 32)
}

func dmv4AADBytes(senderHex, recipientHex, clientMessageID string, threadID *string) []byte {
	// encoding/json emits fields in struct order with no whitespace, matching JS JSON.stringify.
	b, _ := json.Marshal(dmv4AAD{V: DMv4Version, S: strings.ToLower(senderHex), R: strings.ToLower(recipientHex), CID: clientMessageID, TID: threadID})
	return b
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// EncryptDMv4 encrypts plaintext from us (myPriv) to recipientPubHex.
// salt and iv may be nil to generate random ones (non-nil only for tests).
func EncryptDMv4(myPriv []byte, recipientPubHex, clientMessageID string, threadID *string, plaintext string, salt, iv []byte) (*E2EEPayload, error) {
	if clientMessageID == "" {
		return nil, errors.New("v4 DM needs a client message id")
	}
	myPub, err := curve25519.X25519(myPriv, curve25519.Basepoint)
	if err != nil {
		return nil, err
	}
	senderHex := hex.EncodeToString(myPub)
	recipientPubHex = strings.ToLower(recipientPubHex)
	if salt == nil {
		salt = make([]byte, 16)
		_, _ = rand.Read(salt)
	}
	if iv == nil {
		iv = make([]byte, 12)
		_, _ = rand.Read(iv)
	}
	key, err := dmv4Key(myPriv, senderHex, recipientPubHex, salt)
	if err != nil {
		return nil, err
	}
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	ct := aead.Seal(nil, iv, []byte(plaintext), dmv4AADBytes(senderHex, recipientPubHex, clientMessageID, threadID))
	return &E2EEPayload{
		Version:            DMv4Version,
		Algorithm:          DMv4Algorithm,
		SenderPublicKey:    senderHex,
		RecipientPublicKey: recipientPubHex,
		Nonce:              base64.StdEncoding.EncodeToString(salt) + "." + base64.StdEncoding.EncodeToString(iv),
		Ciphertext:         base64.StdEncoding.EncodeToString(ct),
	}, nil
}

// DecryptDMv4 decrypts a payload we sent or received.
func DecryptDMv4(myPriv []byte, p *E2EEPayload, clientMessageID string, threadID *string) (string, error) {
	if p.Version != DMv4Version || p.Algorithm != DMv4Algorithm {
		return "", fmt.Errorf("unsupported DM payload version %d/%s", p.Version, p.Algorithm)
	}
	saltB64, ivB64, ok := strings.Cut(p.Nonce, ".")
	if !ok {
		return "", errors.New("invalid DM nonce format")
	}
	salt, err1 := base64.StdEncoding.DecodeString(saltB64)
	iv, err2 := base64.StdEncoding.DecodeString(ivB64)
	ct, err3 := base64.StdEncoding.DecodeString(p.Ciphertext)
	if err := errors.Join(err1, err2, err3); err != nil {
		return "", fmt.Errorf("invalid DM payload encoding: %w", err)
	}
	if len(salt) != 16 || len(iv) != 12 {
		return "", errors.New("invalid DM salt/nonce length")
	}
	key, err := dmv4Key(myPriv, p.SenderPublicKey, p.RecipientPublicKey, salt)
	if err != nil {
		return "", err
	}
	aead, err := gcm(key)
	if err != nil {
		return "", err
	}
	pt, err := aead.Open(nil, iv, ct, dmv4AADBytes(p.SenderPublicKey, p.RecipientPublicKey, clientMessageID, threadID))
	if err != nil {
		return "", fmt.Errorf("DM decryption failed: %w", err)
	}
	return string(pt), nil
}
