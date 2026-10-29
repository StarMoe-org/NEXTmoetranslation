package backup

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	backupEncryptionKeyEnv  = "MOESEKAI_BACKUP_ENCRYPTION_KEY"
	backupEnvelopeFilename  = "backup.enc"
	backupEnvelopeMediaType = "application/octet-stream"

	backupEnvelopeMagic                = "MOEBAK01"
	backupEnvelopeVersion              = byte(1)
	backupEnvelopeKeyWrapAlgorithm     = byte(1) // AES-256-GCM
	backupEnvelopeDataAlgorithm        = byte(1) // Legacy single-shot AES-256-GCM
	backupEnvelopeDataAlgorithmChunked = byte(2) // Streaming 1 MiB chunked AES-256-GCM
	backupEnvelopeFlags                = byte(0)

	backupEnvelopeChunkSize             = 1 << 20
	backupEnvelopeKeyIDSize             = 16
	backupEnvelopeNonceSize             = 12
	backupEnvelopeDataKeySize           = 32
	backupEnvelopeAuthenticationTagSize = 16

	backupEnvelopeVersionOffset       = len(backupEnvelopeMagic)
	backupEnvelopeKeyWrapOffset       = backupEnvelopeVersionOffset + 1
	backupEnvelopeDataAlgorithmOffset = backupEnvelopeKeyWrapOffset + 1
	backupEnvelopeFlagsOffset         = backupEnvelopeDataAlgorithmOffset + 1
	backupEnvelopeKeyIDOffset         = backupEnvelopeFlagsOffset + 1
	backupEnvelopeWrapNonceOffset     = backupEnvelopeKeyIDOffset + backupEnvelopeKeyIDSize
	backupEnvelopeDataNonceOffset     = backupEnvelopeWrapNonceOffset + backupEnvelopeNonceSize
	backupEnvelopePlaintextSizeOffset = backupEnvelopeDataNonceOffset + backupEnvelopeNonceSize
	backupEnvelopePrefixSize          = backupEnvelopePlaintextSizeOffset + 8
	backupEnvelopeWrappedKeySize      = backupEnvelopeDataKeySize + backupEnvelopeAuthenticationTagSize
	backupEnvelopeHeaderSize          = backupEnvelopePrefixSize + backupEnvelopeWrappedKeySize
	backupEnvelopeChunkAADSize        = backupEnvelopeHeaderSize + 8 + 1
	maxBackupEnvelopeChunks           = (maxS3ResponseBytes + backupEnvelopeChunkSize - 1) / backupEnvelopeChunkSize
	maxBackupEnvelopeBytes            = backupEnvelopeHeaderSize + maxS3ResponseBytes + maxBackupEnvelopeChunks*backupEnvelopeAuthenticationTagSize
)

var ErrBackupEnvelopeAuthentication = errors.New("backup envelope authentication failed")

func loadBackupEncryptionKey() ([]byte, error) {
	raw, present := os.LookupEnv(backupEncryptionKeyEnv)
	if !present || raw == "" {
		return nil, nil
	}
	if raw != strings.TrimSpace(raw) {
		return nil, fmt.Errorf("invalid %s: surrounding whitespace is not allowed", backupEncryptionKeyEnv)
	}
	key, err := base64.StdEncoding.Strict().DecodeString(raw)
	if err != nil || base64.StdEncoding.EncodeToString(key) != raw || len(key) != backupEnvelopeDataKeySize {
		clear(key)
		return nil, fmt.Errorf("invalid %s: must be the canonical base64 encoding of exactly 32 bytes", backupEncryptionKeyEnv)
	}
	return key, nil
}

func chunkedCiphertextSize(plaintextSize uint64) uint64 {
	if plaintextSize == 0 {
		return backupEnvelopeAuthenticationTagSize
	}
	chunks := (plaintextSize + backupEnvelopeChunkSize - 1) / backupEnvelopeChunkSize
	return plaintextSize + chunks*backupEnvelopeAuthenticationTagSize
}

func deriveBackupChunkNonce(baseNonce []byte, chunkIndex uint64) [backupEnvelopeNonceSize]byte {
	var nonce [backupEnvelopeNonceSize]byte
	copy(nonce[:], baseNonce)
	var indexBytes [8]byte
	binary.BigEndian.PutUint64(indexBytes[:], chunkIndex)
	for i := 0; i < 8; i++ {
		nonce[4+i] ^= indexBytes[i]
	}
	return nonce
}

func buildBackupChunkAAD(dst *[backupEnvelopeChunkAADSize]byte, header []byte, chunkIndex uint64, isFinal byte) []byte {
	copy(dst[:backupEnvelopeHeaderSize], header)
	binary.BigEndian.PutUint64(dst[backupEnvelopeHeaderSize:backupEnvelopeHeaderSize+8], chunkIndex)
	dst[backupEnvelopeHeaderSize+8] = isFinal
	return dst[:]
}

func encryptBackupEnvelope(plaintext, keyEncryptionKey []byte) ([]byte, error) {
	if len(keyEncryptionKey) != backupEnvelopeDataKeySize {
		return nil, errors.New("backup key-encryption key must be 32 bytes")
	}
	if len(plaintext) > maxS3ResponseBytes {
		return nil, fmt.Errorf("backup payload exceeds %d bytes", maxS3ResponseBytes)
	}
	expectedLen := int(uint64(backupEnvelopeHeaderSize) + chunkedCiphertextSize(uint64(len(plaintext))))
	var out bytes.Buffer
	out.Grow(expectedLen)
	if err := encryptBackupEnvelopeStream(context.Background(), &out, bytes.NewReader(plaintext), int64(len(plaintext)), keyEncryptionKey); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func encryptBackupEnvelopeFile(ctx context.Context, dstPath, srcPath string, keyEncryptionKey []byte) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("backup source payload is not a regular file")
	}
	tmpPath := dstPath + ".tmp"
	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = encryptBackupEnvelopeStream(ctx, dst, src, info.Size(), keyEncryptionKey)
	closeErr := dst.Close()
	if err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return closeErr
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func encryptBackupEnvelopeStream(ctx context.Context, dst io.Writer, src io.Reader, plaintextSize int64, keyEncryptionKey []byte) error {
	if len(keyEncryptionKey) != backupEnvelopeDataKeySize {
		return errors.New("backup key-encryption key must be 32 bytes")
	}
	if plaintextSize < 0 || plaintextSize > maxS3ResponseBytes {
		return fmt.Errorf("backup payload exceeds %d bytes", maxS3ResponseBytes)
	}

	dataKey := make([]byte, backupEnvelopeDataKeySize)
	defer clear(dataKey)
	if _, err := io.ReadFull(rand.Reader, dataKey); err != nil {
		return fmt.Errorf("generate backup data key: %w", err)
	}
	wrapNonce := make([]byte, backupEnvelopeNonceSize)
	if _, err := io.ReadFull(rand.Reader, wrapNonce); err != nil {
		return fmt.Errorf("generate backup wrap nonce: %w", err)
	}
	baseDataNonce := make([]byte, backupEnvelopeNonceSize)
	if _, err := io.ReadFull(rand.Reader, baseDataNonce); err != nil {
		return fmt.Errorf("generate backup data nonce: %w", err)
	}

	keyIDDigest := sha256.Sum256(keyEncryptionKey)
	defer clear(keyIDDigest[:])

	var prefix [backupEnvelopePrefixSize]byte
	copy(prefix[:len(backupEnvelopeMagic)], backupEnvelopeMagic)
	prefix[backupEnvelopeVersionOffset] = backupEnvelopeVersion
	prefix[backupEnvelopeKeyWrapOffset] = backupEnvelopeKeyWrapAlgorithm
	prefix[backupEnvelopeDataAlgorithmOffset] = backupEnvelopeDataAlgorithmChunked
	prefix[backupEnvelopeFlagsOffset] = backupEnvelopeFlags
	copy(prefix[backupEnvelopeKeyIDOffset:backupEnvelopeWrapNonceOffset], keyIDDigest[:backupEnvelopeKeyIDSize])
	copy(prefix[backupEnvelopeWrapNonceOffset:backupEnvelopeDataNonceOffset], wrapNonce)
	copy(prefix[backupEnvelopeDataNonceOffset:backupEnvelopePlaintextSizeOffset], baseDataNonce)
	binary.BigEndian.PutUint64(prefix[backupEnvelopePlaintextSizeOffset:backupEnvelopePrefixSize], uint64(plaintextSize))

	wrapGCM, err := newAES256GCM(keyEncryptionKey)
	if err != nil {
		return fmt.Errorf("initialize backup key-wrap cipher: %w", err)
	}
	wrappedDataKey := wrapGCM.Seal(nil, wrapNonce, dataKey, prefix[:])
	if len(wrappedDataKey) != backupEnvelopeWrappedKeySize {
		clear(wrappedDataKey)
		return errors.New("unexpected wrapped backup data key length")
	}
	defer clear(wrappedDataKey)

	var header [backupEnvelopeHeaderSize]byte
	copy(header[:backupEnvelopePrefixSize], prefix[:])
	copy(header[backupEnvelopePrefixSize:], wrappedDataKey)
	if _, err := dst.Write(header[:]); err != nil {
		return err
	}

	dataGCM, err := newAES256GCM(dataKey)
	if err != nil {
		return fmt.Errorf("initialize backup payload cipher: %w", err)
	}

	var aadBuf [backupEnvelopeChunkAADSize]byte
	if plaintextSize == 0 {
		nonce := deriveBackupChunkNonce(baseDataNonce, 0)
		aad := buildBackupChunkAAD(&aadBuf, header[:], 0, 1)
		sealed := dataGCM.Seal(nil, nonce[:], nil, aad)
		_, err := dst.Write(sealed)
		clear(sealed)
		return err
	}

	plainBuf := make([]byte, backupEnvelopeChunkSize)
	defer clear(plainBuf)
	cipherBuf := make([]byte, 0, backupEnvelopeChunkSize+backupEnvelopeAuthenticationTagSize)
	defer clear(cipherBuf[:cap(cipherBuf)])

	remaining := plaintextSize
	var chunkIndex uint64
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := backupEnvelopeChunkSize
		if remaining < int64(want) {
			want = int(remaining)
		}
		isFinal := byte(0)
		if int64(want) == remaining {
			isFinal = 1
		}
		if _, err := io.ReadFull(src, plainBuf[:want]); err != nil {
			return err
		}
		nonce := deriveBackupChunkNonce(baseDataNonce, chunkIndex)
		aad := buildBackupChunkAAD(&aadBuf, header[:], chunkIndex, isFinal)
		sealed := dataGCM.Seal(cipherBuf[:0], nonce[:], plainBuf[:want], aad)
		if _, err := dst.Write(sealed); err != nil {
			return err
		}
		remaining -= int64(want)
		chunkIndex++
	}
	var extra [1]byte
	if n, err := src.Read(extra[:]); n > 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return errors.New("backup source payload size changed during encryption")
	}
	return nil
}

func decryptBackupEnvelope(envelope, keyEncryptionKey []byte) ([]byte, error) {
	if len(keyEncryptionKey) != backupEnvelopeDataKeySize {
		return nil, errors.New("backup key-encryption key must be 32 bytes")
	}
	if len(envelope) < backupEnvelopeHeaderSize || len(envelope) > maxBackupEnvelopeBytes {
		return nil, ErrBackupEnvelopeAuthentication
	}
	plaintextSize := binary.BigEndian.Uint64(envelope[backupEnvelopePlaintextSizeOffset:backupEnvelopePrefixSize])
	if plaintextSize > maxS3ResponseBytes {
		return nil, ErrBackupEnvelopeAuthentication
	}
	var out bytes.Buffer
	out.Grow(int(plaintextSize))
	if err := decryptBackupEnvelopeStream(context.Background(), &out, bytes.NewReader(envelope), int64(len(envelope)), keyEncryptionKey); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func decryptBackupEnvelopeFile(ctx context.Context, dstPath, srcPath string, keyEncryptionKey []byte) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return ErrBackupEnvelopeAuthentication
	}
	tmpPath := dstPath + ".tmp"
	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = decryptBackupEnvelopeStream(ctx, dst, src, info.Size(), keyEncryptionKey)
	closeErr := dst.Close()
	if err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return closeErr
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func decryptBackupEnvelopeStream(ctx context.Context, dst io.Writer, src io.Reader, envelopeSize int64, keyEncryptionKey []byte) error {
	if len(keyEncryptionKey) != backupEnvelopeDataKeySize {
		return errors.New("backup key-encryption key must be 32 bytes")
	}
	if envelopeSize < int64(backupEnvelopeHeaderSize) || envelopeSize > int64(maxBackupEnvelopeBytes) {
		return ErrBackupEnvelopeAuthentication
	}
	var header [backupEnvelopeHeaderSize]byte
	if _, err := io.ReadFull(src, header[:]); err != nil {
		return ErrBackupEnvelopeAuthentication
	}
	if string(header[:len(backupEnvelopeMagic)]) != backupEnvelopeMagic {
		return ErrBackupEnvelopeAuthentication
	}
	dataAlg := header[backupEnvelopeDataAlgorithmOffset]
	if header[backupEnvelopeVersionOffset] != backupEnvelopeVersion ||
		header[backupEnvelopeKeyWrapOffset] != backupEnvelopeKeyWrapAlgorithm ||
		(dataAlg != backupEnvelopeDataAlgorithm && dataAlg != backupEnvelopeDataAlgorithmChunked) ||
		header[backupEnvelopeFlagsOffset] != backupEnvelopeFlags {
		return ErrBackupEnvelopeAuthentication
	}

	expectedKeyID := sha256.Sum256(keyEncryptionKey)
	defer clear(expectedKeyID[:])
	keyID := header[backupEnvelopeKeyIDOffset:backupEnvelopeWrapNonceOffset]
	if subtle.ConstantTimeCompare(keyID, expectedKeyID[:backupEnvelopeKeyIDSize]) != 1 {
		return ErrBackupEnvelopeAuthentication
	}

	plaintextSize := binary.BigEndian.Uint64(header[backupEnvelopePlaintextSizeOffset:backupEnvelopePrefixSize])
	if plaintextSize > maxS3ResponseBytes {
		return ErrBackupEnvelopeAuthentication
	}
	var expectedEnvelopeSize uint64
	if dataAlg == backupEnvelopeDataAlgorithm {
		expectedEnvelopeSize = uint64(backupEnvelopeHeaderSize) + plaintextSize + uint64(backupEnvelopeAuthenticationTagSize)
	} else {
		expectedEnvelopeSize = uint64(backupEnvelopeHeaderSize) + chunkedCiphertextSize(plaintextSize)
	}
	if uint64(envelopeSize) != expectedEnvelopeSize {
		return ErrBackupEnvelopeAuthentication
	}

	wrapGCM, err := newAES256GCM(keyEncryptionKey)
	if err != nil {
		return fmt.Errorf("initialize backup key-wrap cipher: %w", err)
	}
	wrapNonce := header[backupEnvelopeWrapNonceOffset:backupEnvelopeDataNonceOffset]
	wrappedDataKey := header[backupEnvelopePrefixSize:backupEnvelopeHeaderSize]
	dataKey, err := wrapGCM.Open(nil, wrapNonce, wrappedDataKey, header[:backupEnvelopePrefixSize])
	if err != nil || len(dataKey) != backupEnvelopeDataKeySize {
		clear(dataKey)
		return ErrBackupEnvelopeAuthentication
	}
	defer clear(dataKey)

	dataGCM, err := newAES256GCM(dataKey)
	if err != nil {
		return fmt.Errorf("initialize backup payload cipher: %w", err)
	}
	baseDataNonce := header[backupEnvelopeDataNonceOffset:backupEnvelopePlaintextSizeOffset]

	if dataAlg == backupEnvelopeDataAlgorithm {
		cipherLen := int(envelopeSize - int64(backupEnvelopeHeaderSize))
		ciphertext := make([]byte, cipherLen)
		defer clear(ciphertext)
		if _, err := io.ReadFull(src, ciphertext); err != nil {
			return ErrBackupEnvelopeAuthentication
		}
		plaintext, err := dataGCM.Open(nil, baseDataNonce, ciphertext, header[:])
		if err != nil {
			clear(plaintext)
			return ErrBackupEnvelopeAuthentication
		}
		defer clear(plaintext)
		if uint64(len(plaintext)) != plaintextSize {
			return ErrBackupEnvelopeAuthentication
		}
		_, err = dst.Write(plaintext)
		return err
	}

	var aadBuf [backupEnvelopeChunkAADSize]byte
	if plaintextSize == 0 {
		var tag [backupEnvelopeAuthenticationTagSize]byte
		if _, err := io.ReadFull(src, tag[:]); err != nil {
			return ErrBackupEnvelopeAuthentication
		}
		nonce := deriveBackupChunkNonce(baseDataNonce, 0)
		aad := buildBackupChunkAAD(&aadBuf, header[:], 0, 1)
		plaintext, err := dataGCM.Open(nil, nonce[:], tag[:], aad)
		clear(plaintext)
		if err != nil || len(plaintext) != 0 {
			return ErrBackupEnvelopeAuthentication
		}
		return nil
	}

	cipherBuf := make([]byte, backupEnvelopeChunkSize+backupEnvelopeAuthenticationTagSize)
	defer clear(cipherBuf)
	plainBuf := make([]byte, 0, backupEnvelopeChunkSize)
	defer clear(plainBuf[:cap(plainBuf)])

	remaining := int64(plaintextSize)
	var chunkIndex uint64
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		plainWant := backupEnvelopeChunkSize
		if remaining < int64(plainWant) {
			plainWant = int(remaining)
		}
		isFinal := byte(0)
		if int64(plainWant) == remaining {
			isFinal = 1
		}
		cipherWant := plainWant + backupEnvelopeAuthenticationTagSize
		if _, err := io.ReadFull(src, cipherBuf[:cipherWant]); err != nil {
			return ErrBackupEnvelopeAuthentication
		}
		nonce := deriveBackupChunkNonce(baseDataNonce, chunkIndex)
		aad := buildBackupChunkAAD(&aadBuf, header[:], chunkIndex, isFinal)
		plain, err := dataGCM.Open(plainBuf[:0], nonce[:], cipherBuf[:cipherWant], aad)
		if err != nil || len(plain) != plainWant {
			return ErrBackupEnvelopeAuthentication
		}
		if _, err := dst.Write(plain); err != nil {
			return err
		}
		remaining -= int64(plainWant)
		chunkIndex++
	}
	var extra [1]byte
	if n, err := src.Read(extra[:]); n > 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return ErrBackupEnvelopeAuthentication
	}
	return nil
}

func newAES256GCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
