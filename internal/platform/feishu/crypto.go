package feishu

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

// decryptFeishuEvent decrypts an Encrypt-Key-mode push body per the official
// SDK scheme: key = SHA-256(encryptKey), IV = first 16 bytes, AES-256-CBC,
// PKCS#7 unpad. Callers MUST verify the signature on the raw (still encrypted)
// body first — that is what Feishu signs.
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
