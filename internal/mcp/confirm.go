package mcp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ConfirmTTL bounds how long a pending human confirmation stays valid.
const ConfirmTTL = 10 * time.Minute

// confirmState is carried by the client between the input_required result and the retry
// (MRTR requestState). It is HMAC-signed, so the client cannot change what was confirmed.
type confirmState struct {
	Tool    string `json:"t"`
	Args    string `json:"a"` // sha256 of the canonical arguments
	Caller  string `json:"c"`
	Expires int64  `json:"e"`
	Nonce   string `json:"n"` // also the action's idempotency key
}

var errBadState = errors.New("the confirmation is invalid or has expired; call the tool again")

// argsHash fingerprints arguments; encoding/json sorts map keys, so equal arguments hash equally.
func argsHash(args map[string]any) string {
	b, _ := json.Marshal(args)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sign(secret []byte, payload string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func newState(secret []byte, tool string, args map[string]any, caller string, now time.Time) (string, confirmState) {
	var n [16]byte
	_, _ = rand.Read(n[:])
	st := confirmState{Tool: tool, Args: argsHash(args), Caller: caller, Expires: now.Add(ConfirmTTL).Unix(), Nonce: hex.EncodeToString(n[:])}
	b, _ := json.Marshal(st)
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + sign(secret, payload), st
}

// verifyState checks signature, expiry, and that tool, arguments and caller are unchanged.
func verifyState(secret []byte, token, tool string, args map[string]any, caller string, now time.Time) (*confirmState, error) {
	payload, mac, ok := strings.Cut(token, ".")
	if !ok || !hmac.Equal([]byte(sign(secret, payload)), []byte(mac)) {
		return nil, errBadState
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil, errBadState
	}
	var st confirmState
	if json.Unmarshal(b, &st) != nil || now.Unix() > st.Expires {
		return nil, errBadState
	}
	if st.Tool != tool || st.Caller != caller || st.Args != argsHash(args) {
		return nil, errBadState
	}
	return &st, nil
}
