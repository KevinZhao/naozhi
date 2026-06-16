package feishu

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

// decryptFeishuEvent decrypts a Feishu webhook payload that was encrypted with
// the app's Encrypt Key. When an Encrypt Key is configured in the Feishu event
// subscription settings, Feishu wraps the ENTIRE push body as
// `{"encrypt":"<base64>"}` and the operator's handler must AES-decrypt the
// inner ciphertext before it can read the challenge handshake or any event.
//
// Scheme (matches the official Lark/Feishu SDK EventDispatcher and the
// "Encrypt Key" docs):
//
//   - key  = SHA-256(encryptKey)                 (32 bytes → AES-256)
//   - data = base64-decode(encrypt)
//   - IV   = data[:aes.BlockSize]                (first 16 bytes)
//   - ct   = data[aes.BlockSize:]                (AES-256-CBC ciphertext)
//   - plaintext = PKCS#7-unpadded CBC-decrypt(ct)
//
// Signature verification (verifySignature) runs over the RAW request body
// BEFORE this step, exactly as Feishu computes it (SHA256(ts+nonce+key+body)),
// so callers must verify the signature on the undecrypted body first.
func decryptFeishuEvent(encryptKey, encrypt string) ([]byte, error) {
	if encryptKey == "" {
		return nil, errors.New("feishu decrypt: empty encrypt key")
	}
	buf, err := base64.StdEncoding.DecodeString(encrypt)
	if err != nil {
		return nil, err
	}
	if len(buf) < aes.BlockSize {
		return nil, errors.New("feishu decrypt: ciphertext too short")
	}
	key := sha256.Sum256([]byte(encryptKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	iv := buf[:aes.BlockSize]
	ct := buf[aes.BlockSize:]
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, errors.New("feishu decrypt: ciphertext not a multiple of block size")
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
	return pkcs7Unpad(pt)
}

// pkcs7Unpad removes PKCS#7 padding, validating the padding bytes to reject
// malformed/tampered ciphertext rather than silently returning garbage.
func pkcs7Unpad(b []byte) ([]byte, error) {
	n := len(b)
	if n == 0 || n%aes.BlockSize != 0 {
		return nil, errors.New("feishu decrypt: invalid padded length")
	}
	pad := int(b[n-1])
	if pad == 0 || pad > aes.BlockSize || pad > n {
		return nil, errors.New("feishu decrypt: invalid padding size")
	}
	for _, c := range b[n-pad:] {
		if int(c) != pad {
			return nil, errors.New("feishu decrypt: invalid padding bytes")
		}
	}
	return b[:n-pad], nil
}
