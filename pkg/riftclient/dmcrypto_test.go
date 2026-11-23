package riftclient

import (
	"bytes"
	"encoding/hex"
	"os"
	"testing"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Vector from https://therift.chat/encryption/ ("A message").
func TestDMv4Vector(t *testing.T) {
	senderPriv := bytes.Repeat([]byte{0x11}, 32)
	recipientPriv := bytes.Repeat([]byte{0x22}, 32)
	const (
		senderPub    = "7b4e909bbe7ffe44c465a220037d608ee35897d31ef972f07f74892cb0f73f13"
		recipientPub = "0faa684ed28867b97f4a6a2dee5df8ce974e76b7018e3f22a1c4cf2678570f20"
		cid          = "0f1e2d3c-4b5a-4978-8695-a4b3c2d1e0f9"
	)
	salt := bytes.Repeat([]byte{0x33}, 16)
	iv := bytes.Repeat([]byte{0x44}, 12)

	key, err := dmv4Key(senderPriv, senderPub, recipientPub, salt)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(key); got != "ad38ea32e3031e25640b42632275e6f6e948676de386d0ce503261ec30c76f57" {
		t.Fatalf("message key = %s", got)
	}
	wantAAD := `{"v":4,"s":"` + senderPub + `","r":"` + recipientPub + `","cid":"` + cid + `","tid":null}`
	if got := string(dmv4AADBytes(senderPub, recipientPub, cid, nil)); got != wantAAD {
		t.Fatalf("aad = %s", got)
	}

	p, err := EncryptDMv4(senderPriv, recipientPub, cid, nil, "hello", salt, iv)
	if err != nil {
		t.Fatal(err)
	}
	if p.SenderPublicKey != senderPub {
		t.Fatalf("sender pub = %s", p.SenderPublicKey)
	}
	if p.Nonce != "MzMzMzMzMzMzMzMzMzMzMw==.RERERERERERERERE" || p.Ciphertext != "7rXLwSPpN4hbbQJIURxFWiWZ7iSQ" {
		t.Fatalf("wire nonce/ciphertext = %s / %s", p.Nonce, p.Ciphertext)
	}

	for name, priv := range map[string][]byte{"recipient": recipientPriv, "sender": senderPriv} {
		pt, err := DecryptDMv4(priv, p, cid, nil)
		if err != nil || pt != "hello" {
			t.Fatalf("%s decrypt = %q, %v", name, pt, err)
		}
	}
	if _, err := DecryptDMv4(recipientPriv, p, "other-id", nil); err == nil {
		t.Fatal("decrypt with wrong message id should fail")
	}
}

// Checks key derivation against a real account without committing secrets:
// RIFT_TEST_MNEMONIC="..." RIFT_TEST_DM_PUB=<identity_key hex> go test ./pkg/riftclient -run TestDeriveRealAccount
func TestDeriveRealAccount(t *testing.T) {
	mnemonic, want := os.Getenv("RIFT_TEST_MNEMONIC"), os.Getenv("RIFT_TEST_DM_PUB")
	if mnemonic == "" || want == "" {
		t.Skip("RIFT_TEST_MNEMONIC / RIFT_TEST_DM_PUB not set")
	}
	k, err := DeriveKeysFromMnemonic(mnemonic)
	if err != nil {
		t.Fatal(err)
	}
	if k.DMPublicHex() != want {
		t.Fatalf("derived DM public key %s, want %s", k.DMPublicHex(), want)
	}
	if spk := os.Getenv("RIFT_TEST_SPK_PUB"); spk != "" && k.SignedPrekeyPublicHex() != spk {
		t.Fatalf("derived signed prekey %s, want %s", k.SignedPrekeyPublicHex(), spk)
	}
}
