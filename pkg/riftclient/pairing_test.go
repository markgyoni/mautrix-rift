package riftclient

import (
	"encoding/hex"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Test vector from therift.chat/encryption ("Adding a device").
func TestPairingVector(t *testing.T) {
	req, err := newPairingRequest("583465893", mustHex(t, "5402d700f2f01a5c1a0a5eb07d977a1265cbb53b303a10425f00ae1f47ef1e0e"))
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(req.Share); got != "ba33d2d13a2849271150b9022a01acd02c60800852f93a694b012aef099ae550" {
		t.Fatalf("share = %s", got)
	}
	approver := mustHex(t, "0c1510bd818aee9b72c9323a35d7fc60f4748d575eb7e5a36ff93fdbd0565276")
	isk, err := req.sessionKey(approver)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(isk); got != "abdd4024f2b148143ce78d8446820850ae015a645848c933d2a1ebb249e13506efacc0b45bf88aad6e71656d197dca74f877a1409aa2ecc4628495e1d2b15f13" {
		t.Fatalf("ISK = %s", got)
	}
	pt, err := req.DecryptBundle("3F2B9C1E-7A54-4D0A-9B1C-5E6F7A8B9C0D", approver,
		mustHex(t, "27ededf203af3bea32b6fabc"),
		mustHex(t, "da975cdc44ab5145f245638b6e45c3850eea21643fac8005cd38ac3849550b7935635d540ec1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(pt) != `{"v":1,"hello":"rift"}` {
		t.Fatalf("plaintext = %q", pt)
	}

	wrong, _ := newPairingRequest("583465894", mustHex(t, "5402d700f2f01a5c1a0a5eb07d977a1265cbb53b303a10425f00ae1f47ef1e0e"))
	if _, err := wrong.DecryptBundle("3f2b9c1e-7a54-4d0a-9b1c-5e6f7a8b9c0d", approver,
		mustHex(t, "27ededf203af3bea32b6fabc"),
		mustHex(t, "da975cdc44ab5145f245638b6e45c3850eea21643fac8005cd38ac3849550b7935635d540ec1")); err != ErrPairingCodeMismatch {
		t.Fatalf("wrong code: err = %v", err)
	}
}

func TestPairingCode(t *testing.T) {
	if d := luhnCheckDigit("58346589"); d != 3 {
		t.Fatalf("check digit = %d, want 3", d)
	}
	for range 50 {
		req, err := NewPairingRequest()
		if err != nil {
			t.Fatal(err)
		}
		if len(req.Code) != PairingCodeLength || luhnCheckDigit(req.Code[:8]) != req.Code[8]-'0' {
			t.Fatalf("bad code %q", req.Code)
		}
	}
}
