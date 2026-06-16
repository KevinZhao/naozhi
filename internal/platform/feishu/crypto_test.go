package feishu

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

// encryptFeishu mirrors Feishu's Encrypt Key wire format so the test can
// produce ciphertext exactly the way the upstream server would:
//
//	key = SHA256(encryptKey); AES-256-CBC; output = base64(IV || ciphertext)
//
// IV is a fixed deterministic block here purely for test repeatability; the
// real server uses a random IV, which decryptFeishuEvent reads from the first
// 16 bytes regardless.
func encryptFeishu(t *testing.T, encryptKey string, plaintext []byte) string {
	t.Helper()
	key := sha256.Sum256([]byte(encryptKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	iv := make([]byte, aes.BlockSize)
	for i := range iv {
		iv[i] = byte(i + 1)
	}
	// PKCS#7 pad.
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := append(append([]byte{}, plaintext...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	out := append(append([]byte{}, iv...), ct...)
	return base64.StdEncoding.EncodeToString(out)
}

func TestDecryptFeishuEvent(t *testing.T) {
	t.Parallel()
	const key = "my-test-encrypt-key-1234567890"

	t.Run("round trip challenge", func(t *testing.T) {
		t.Parallel()
		plain := []byte(`{"challenge":"abc123","token":"tok","type":"url_verification"}`)
		enc := encryptFeishu(t, key, plain)
		got, err := decryptFeishuEvent(key, enc)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("round trip mismatch:\n got=%q\nwant=%q", got, plain)
		}
	})

	t.Run("round trip event payload", func(t *testing.T) {
		t.Parallel()
		plain := []byte(`{"schema":"2.0","header":{"event_type":"im.message.receive_v1","token":"tok"},"event":{"message":{"message_id":"m1"}}}`)
		enc := encryptFeishu(t, key, plain)
		got, err := decryptFeishuEvent(key, enc)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("round trip mismatch:\n got=%q\nwant=%q", got, plain)
		}
	})

	t.Run("errors", func(t *testing.T) {
		t.Parallel()
		valid := encryptFeishu(t, key, []byte(`{"x":1}`))
		tests := []struct {
			name       string
			key        string
			encrypt    string
			wantErr    bool
			wantOKWith string // if non-empty, expects success and compares plaintext
		}{
			{name: "empty key", key: "", encrypt: valid, wantErr: true},
			{name: "not base64", key: key, encrypt: "!!!not base64!!!", wantErr: true},
			{name: "too short", key: key, encrypt: base64.StdEncoding.EncodeToString([]byte("short")), wantErr: true},
			{name: "wrong key", key: "different-key", encrypt: valid, wantErr: true}, // bad PKCS#7 pad
			{name: "valid", key: key, encrypt: valid, wantErr: false, wantOKWith: `{"x":1}`},
		}
		for _, tc := range tests {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				got, err := decryptFeishuEvent(tc.key, tc.encrypt)
				if tc.wantErr {
					if err == nil {
						t.Fatalf("expected error, got plaintext %q", got)
					}
					return
				}
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if tc.wantOKWith != "" && string(got) != tc.wantOKWith {
					t.Fatalf("plaintext = %q, want %q", got, tc.wantOKWith)
				}
			})
		}
	})
}
