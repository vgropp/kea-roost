// Package auth keeps the users of the interface, their sessions and the limits on
// failed logins.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id with the parameters OWASP recommends for a small server: 19 MiB, two
// passes, one thread.
const (
	argonMemory  = 19 * 1024
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

const (
	// MinPasswordLength follows the length NIST asks for when there is no second factor.
	MinPasswordLength = 12
	maxPasswordLength = 256
)

var errBadHash = errors.New("unknown password hash format")

// HashPassword returns a hash in the PHC string format.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// checkPassword verifies a password against a hash made by HashPassword. The
// parameters are read from the hash, so that older hashes keep working.
func checkPassword(hash, password string) (bool, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errBadHash
	}
	var version int
	var memory, passes uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errBadHash
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &passes, &threads); err != nil || memory == 0 || passes == 0 || threads == 0 {
		return false, errBadHash
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil {
		return false, errBadHash
	}
	want, err := enc.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, errBadHash
	}
	got := argon2.IDKey([]byte(password), salt, passes, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// CheckPasswordRules returns why a new password is not acceptable.
func CheckPasswordRules(name, password string) error {
	switch {
	case len(password) < MinPasswordLength:
		return fmt.Errorf("the password needs at least %d characters", MinPasswordLength)
	case len(password) > maxPasswordLength:
		return fmt.Errorf("the password has more than %d characters", maxPasswordLength)
	case strings.EqualFold(password, name):
		return errors.New("the password must not be the user name")
	}
	return nil
}

// RandomToken returns a random value for sessions and the setup code.
func RandomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
