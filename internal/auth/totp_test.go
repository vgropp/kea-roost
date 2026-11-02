package auth

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

// The SHA-1 test vectors of RFC 6238, appendix B.
func TestTOTPVectors(t *testing.T) {
	key := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"} {
		if got := totpCode(key, uint64(unix)/totpStep); got != want {
			t.Errorf("%d: %s, want %s", unix, got, want)
		}
	}
}

func TestCheckTOTP(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	now := time.Unix(1111111109, 0)
	step, ok := CheckTOTP(secret, "081804", now, 0)
	if !ok {
		t.Fatal("valid code refused")
	}
	if _, ok := CheckTOTP(secret, "081804", now, step); ok {
		t.Fatal("a used code was accepted again")
	}
	if _, ok := CheckTOTP(secret, "081 804", now.Add(29*time.Second), 0); !ok {
		t.Fatal("a code with a space or a little drift was refused")
	}
	if _, ok := CheckTOTP(secret, "081804", now.Add(2*time.Minute), 0); ok {
		t.Fatal("an old code was accepted")
	}
	for _, bad := range []string{"", "12345", "abcdef", "0818040"} {
		if _, ok := CheckTOTP(secret, bad, now, 0); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, ok := CheckTOTP("not base32!", "081804", now, 0); ok {
		t.Fatal("broken secret accepted")
	}
}

func TestNewTOTPSecretAndURI(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil || len(secret) != 32 {
		t.Fatalf("%q %v", secret, err)
	}
	uri := TOTPURI("Kea Roost", "admin", secret)
	if !strings.HasPrefix(uri, "otpauth://totp/Kea%20Roost:admin?") || !strings.Contains(uri, "secret="+secret) {
		t.Fatalf("uri: %s", uri)
	}
}
