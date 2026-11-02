package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP follows RFC 6238 with the values every authenticator app knows: SHA-1, six
// digits and steps of 30 seconds.
const (
	totpStep   = 30
	totpDigits = 6
	// one step of drift either way covers clocks that are a little off
	totpDrift = 1
)

var base32NoPadding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random secret in the form authenticator apps expect.
func NewTOTPSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base32NoPadding.EncodeToString(b), nil
}

// TOTPURI is what the QR code holds for an authenticator app.
func TOTPURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

func totpCode(key []byte, counter uint64) string {
	mac := hmac.New(sha1.New, key)
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, value%1000000)
}

// CheckTOTP returns the time step of a valid code. A step at or below last was used
// already and is refused, so that a code seen by somebody else cannot be replayed.
func CheckTOTP(secret, code string, now time.Time, last uint64) (uint64, bool) {
	key, err := base32NoPadding.DecodeString(strings.ToUpper(strings.TrimRight(secret, "=")))
	if err != nil || len(key) == 0 {
		return 0, false
	}
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, false
	}
	current := uint64(now.Unix()) / totpStep
	for d := -totpDrift; d <= totpDrift; d++ {
		step := uint64(int64(current) + int64(d))
		if step <= last {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(totpCode(key, step)), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// TOTPAt returns the code an authenticator app shows at a time.
func TOTPAt(secret string, at time.Time) (string, error) {
	key, err := base32NoPadding.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return "", err
	}
	return totpCode(key, uint64(at.Unix())/totpStep), nil
}
