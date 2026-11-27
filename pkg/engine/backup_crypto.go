package engine

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
)

const (
	backupEncMagic      = "VKUBEENC1"
	backupEncSaltLen    = 16
	backupEncNonceLen   = 12
	backupEncPBKDF2Iter = 100000
)

// isEncryptedBackup kiểm tra nhanh file có tiền tố định dạng mã hóa AES-256-GCM của vkube hay không.
func isEncryptedBackup(filePath string) bool {
	f, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer f.Close()
	hdr := make([]byte, len(backupEncMagic))
	n, _ := f.Read(hdr)
	return n == len(backupEncMagic) && string(hdr) == backupEncMagic
}

// encryptBackupFile mã hóa file backup bằng AES-256-GCM (authenticated encryption) phía client với khóa dẫn xuất PBKDF2-HMAC-SHA256.
func encryptBackupFile(srcPath, dstPath, passphrase string) error {
	if passphrase == "" {
		return fmt.Errorf("encryption passphrase is required")
	}
	plain, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}

	salt := make([]byte, backupEncSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("generate salt: %w", err)
	}
	key := pbkdf2SHA256([]byte(passphrase), salt, backupEncPBKDF2Iter, 32)

	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, plain, []byte(backupEncMagic))

	var buf bytes.Buffer
	buf.Grow(len(backupEncMagic) + len(salt) + len(nonce) + len(ciphertext))
	buf.WriteString(backupEncMagic)
	buf.Write(salt)
	buf.Write(nonce)
	buf.Write(ciphertext)

	return os.WriteFile(dstPath, buf.Bytes(), 0o600)
}

// decryptBackupFile giải mã & xác thực toàn vẹn (GCM tag) file backup đã mã hóa bởi encryptBackupFile.
func decryptBackupFile(srcPath, dstPath, passphrase string) error {
	if passphrase == "" {
		return fmt.Errorf("backup archive is encrypted; set VKUBE_BACKUP_PASSPHRASE or spec.backup.encryptionPassphrase")
	}
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}
	minLen := len(backupEncMagic) + backupEncSaltLen + backupEncNonceLen + 16
	if len(raw) < minLen || string(raw[:len(backupEncMagic)]) != backupEncMagic {
		return fmt.Errorf("invalid encrypted backup header")
	}
	off := len(backupEncMagic)
	salt := raw[off : off+backupEncSaltLen]
	off += backupEncSaltLen
	nonce := raw[off : off+backupEncNonceLen]
	off += backupEncNonceLen
	ciphertext := raw[off:]

	key := pbkdf2SHA256([]byte(passphrase), salt, backupEncPBKDF2Iter, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, []byte(backupEncMagic))
	if err != nil {
		return fmt.Errorf("decrypt backup failed (wrong passphrase or archive was tampered): %w", err)
	}
	return os.WriteFile(dstPath, plain, 0o600)
}

// pbkdf2SHA256 cài đặt PBKDF2-HMAC-SHA256 chuẩn RFC 2898 bằng Pure Go stdlib (không cần thư viện ngoài).
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	hashLen := sha256.Size
	numBlocks := (keyLen + hashLen - 1) / hashLen
	dk := make([]byte, 0, numBlocks*hashLen)
	var blockIdx [4]byte

	for block := 1; block <= numBlocks; block++ {
		binary.BigEndian.PutUint32(blockIdx[:], uint32(block))
		mac := hmac.New(sha256.New, password)
		_, _ = mac.Write(salt)
		_, _ = mac.Write(blockIdx[:])
		u := mac.Sum(nil)
		out := make([]byte, len(u))
		copy(out, u)

		for i := 1; i < iter; i++ {
			mac.Reset()
			_, _ = mac.Write(u)
			u = mac.Sum(nil)
			for j := range out {
				out[j] ^= u[j]
			}
		}
		dk = append(dk, out...)
	}
	return dk[:keyLen]
}
