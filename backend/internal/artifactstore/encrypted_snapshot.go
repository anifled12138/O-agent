package artifactstore

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

const (
	controlSnapshotKeyBytes = 32
	backupFrameBytes        = 4 << 20
	backupHeader            = "OABK\x00\x01\r\n"
	backupFrameData         = byte(0)
	backupFrameFinal        = byte(1)
)

// GenerateControlSnapshotKey creates an independent 256-bit random key. Keep
// this key outside the encrypted backup destination; losing it makes recovery
// impossible, and storing it beside the backup defeats encryption.
func GenerateControlSnapshotKey(path string) (retErr error) {
	if strings.TrimSpace(path) == "" {
		return domain.ErrInvalid
	}
	keyPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(keyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	keep := false
	closed := false
	defer func() {
		if !keep {
			var closeErr error
			if !closed {
				closeErr = file.Close()
			}
			retErr = errors.Join(retErr, closeErr, os.Remove(keyPath), syncDirectory(filepath.Dir(keyPath)))
		}
	}()
	key := make([]byte, controlSnapshotKeyBytes)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return err
	}
	written, writeErr := file.Write(key)
	if writeErr == nil && written != len(key) {
		writeErr = io.ErrShortWrite
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	closed = true
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	readBack, err := loadControlSnapshotKey(keyPath)
	if err != nil || !bytes.Equal(readBack, key) {
		return errors.Join(fmt.Errorf("backup key did not read back correctly"), err)
	}
	if err := syncDirectory(filepath.Dir(keyPath)); err != nil {
		return err
	}
	keep = true
	return nil
}

// LoadControlSnapshotKey reads an exact-size regular key file without
// following a final symlink.
func LoadControlSnapshotKey(path string) ([]byte, error) {
	return loadControlSnapshotKey(path)
}

func loadControlSnapshotKey(keyPath string) ([]byte, error) {
	if strings.TrimSpace(keyPath) == "" {
		return nil, domain.ErrInvalid
	}
	info, err := os.Lstat(keyPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != controlSnapshotKeyBytes {
		return nil, fmt.Errorf("control snapshot key must be a regular file containing exactly %d bytes", controlSnapshotKeyBytes)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("control snapshot key permissions must not grant group or other access")
	}
	file, err := os.Open(keyPath)
	if err != nil {
		return nil, err
	}
	key := make([]byte, controlSnapshotKeyBytes)
	_, readErr := io.ReadFull(file, key)
	var extra [1]byte
	extraCount, extraErr := file.Read(extra[:])
	closeErr := file.Close()
	if errors.Is(extraErr, io.EOF) {
		extraErr = nil
	}
	if err := errors.Join(readErr, extraErr, closeErr); err != nil {
		return nil, err
	}
	if extraCount != 0 {
		return nil, fmt.Errorf("control snapshot key contains trailing bytes")
	}
	return key, nil
}

// CreateEncryptedControlSnapshot creates and verifies a complete snapshot,
// then publishes it as a single authenticated encrypted file. The temporary
// plaintext snapshot is removed before this function returns.
func CreateEncryptedControlSnapshot(ctx context.Context, sourceDataDir, destinationFile string, live *storage.Store, key []byte) (manifest ControlSnapshotManifest, retErr error) {
	if len(key) != controlSnapshotKeyBytes || strings.TrimSpace(destinationFile) == "" {
		return ControlSnapshotManifest{}, domain.ErrInvalid
	}
	destination, err := filepath.Abs(destinationFile)
	if err != nil {
		return ControlSnapshotManifest{}, err
	}
	sourceRoot, err := filepath.Abs(sourceDataDir)
	if err != nil {
		return ControlSnapshotManifest{}, err
	}
	if rootsOverlap(sourceRoot, destination) {
		return ControlSnapshotManifest{}, domain.ErrInvalid
	}
	if _, err := os.Lstat(destination); err == nil {
		return ControlSnapshotManifest{}, os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return ControlSnapshotManifest{}, err
	}
	sourceParent := filepath.Dir(sourceRoot)
	snapshotDir, err := os.MkdirTemp(sourceParent, ".o-snapshot-stage-*")
	if err != nil {
		return ControlSnapshotManifest{}, err
	}
	published := false
	defer func() {
		cleanupErr := errors.Join(os.RemoveAll(snapshotDir), syncDirectory(sourceParent))
		if cleanupErr != nil {
			if published {
				cleanupErr = errors.Join(cleanupErr, os.Remove(destination), syncDirectory(filepath.Dir(destination)))
			}
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()
	if err := os.Remove(snapshotDir); err != nil {
		return ControlSnapshotManifest{}, err
	}
	manifest, err = createControlSnapshot(ctx, sourceRoot, snapshotDir, live, true)
	if err != nil {
		return ControlSnapshotManifest{}, err
	}
	if err := EncryptControlSnapshot(ctx, snapshotDir, destination, key); err != nil {
		return ControlSnapshotManifest{}, err
	}
	published = true
	return manifest, nil
}

// EncryptControlSnapshot converts a verified directory snapshot into a
// streaming AES-256-GCM archive without placing plaintext bytes in the output.
func EncryptControlSnapshot(ctx context.Context, snapshotRoot, destinationFile string, key []byte) (retErr error) {
	if len(key) != controlSnapshotKeyBytes || strings.TrimSpace(snapshotRoot) == "" || strings.TrimSpace(destinationFile) == "" {
		return domain.ErrInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	destination, err := filepath.Abs(destinationFile)
	if err != nil {
		return err
	}
	snapshot, err := filepath.Abs(snapshotRoot)
	if err != nil {
		return err
	}
	if rootsOverlap(snapshot, destination) {
		return domain.ErrInvalid
	}
	manifest, err := VerifyControlSnapshot(ctx, snapshot)
	if err != nil {
		return fmt.Errorf("refuse to encrypt an unverified control snapshot: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(destination); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".o-encrypted-snapshot-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		if !keep {
			retErr = errors.Join(retErr, temporary.Close(), os.Remove(temporaryPath), syncDirectory(filepath.Dir(destination)))
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	frames, err := newBackupFrameWriter(temporary, aead)
	if err != nil {
		return err
	}
	if err := writeSnapshotTar(ctx, snapshot, frames, manifest); err != nil {
		return err
	}
	if err := frames.Close(); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	// A same-directory hard link atomically publishes a complete file without
	// replacing a concurrently-created destination.
	if err := os.Link(temporaryPath, destination); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return errors.Join(err, os.Remove(destination))
	}
	if err := os.Remove(temporaryPath); err != nil {
		return errors.Join(err, os.Remove(destination))
	}
	keep = true
	return nil
}

// VerifyEncryptedControlSnapshot authenticates every frame and validates the
// decrypted database, vault key, and referenced objects before returning.
func VerifyEncryptedControlSnapshot(ctx context.Context, encryptedFile string, key []byte) (manifest ControlSnapshotManifest, retErr error) {
	if strings.TrimSpace(encryptedFile) == "" || len(key) != controlSnapshotKeyBytes {
		return ControlSnapshotManifest{}, domain.ErrInvalid
	}
	absPath, err := filepath.Abs(encryptedFile)
	if err != nil {
		return ControlSnapshotManifest{}, err
	}
	temporaryRoot, err := os.MkdirTemp(filepath.Dir(absPath), ".o-snapshot-verify-*")
	if err != nil {
		return ControlSnapshotManifest{}, err
	}
	if err := os.Remove(temporaryRoot); err != nil {
		return ControlSnapshotManifest{}, err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(temporaryRoot)) }()
	if err := DecryptControlSnapshot(ctx, encryptedFile, temporaryRoot, key); err != nil {
		return ControlSnapshotManifest{}, err
	}
	return VerifyControlSnapshot(ctx, temporaryRoot)
}

// DecryptControlSnapshot extracts an authenticated encrypted archive into a
// new snapshot directory and validates its complete contents before success.
func DecryptControlSnapshot(ctx context.Context, encryptedFile, destinationRoot string, key []byte) (retErr error) {
	if strings.TrimSpace(encryptedFile) == "" || strings.TrimSpace(destinationRoot) == "" || len(key) != controlSnapshotKeyBytes {
		return domain.ErrInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	destination, err := filepath.Abs(destinationRoot)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(destination); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, ".o-snapshot-decrypt-*")
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, os.RemoveAll(staging), syncDirectory(parent))
		}
	}()
	if err := extractEncryptedSnapshot(ctx, encryptedFile, staging, key); err != nil {
		return err
	}
	if _, err := VerifyControlSnapshot(ctx, staging); err != nil {
		return fmt.Errorf("decrypted control snapshot failed validation: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(staging, destination); err != nil {
		return err
	}
	if err := syncDirectory(parent); err != nil {
		return errors.Join(err, os.RemoveAll(destination), syncDirectory(parent))
	}
	return nil
}

// RestoreEncryptedControlSnapshot restores a verified encrypted snapshot into
// a new data directory. Existing directories are never overwritten.
func RestoreEncryptedControlSnapshot(ctx context.Context, encryptedFile, destinationDataDir string, key []byte) (retErr error) {
	if strings.TrimSpace(encryptedFile) == "" || strings.TrimSpace(destinationDataDir) == "" || len(key) != controlSnapshotKeyBytes {
		return domain.ErrInvalid
	}
	destination, err := filepath.Abs(destinationDataDir)
	if err != nil {
		return err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	snapshot, err := os.MkdirTemp(parent, ".o-snapshot-restore-source-*")
	if err != nil {
		return err
	}
	if err := os.Remove(snapshot); err != nil {
		return err
	}
	restored := false
	defer func() {
		cleanupErr := os.RemoveAll(snapshot)
		if cleanupErr != nil {
			if restored {
				cleanupErr = errors.Join(cleanupErr, os.RemoveAll(destination), syncDirectory(parent))
			}
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()
	if err := DecryptControlSnapshot(ctx, encryptedFile, snapshot, key); err != nil {
		return err
	}
	if err := RestoreControlSnapshot(ctx, snapshot, destination); err != nil {
		return err
	}
	restored = true
	return nil
}

func writeSnapshotTar(ctx context.Context, snapshotRoot string, destination io.Writer, manifest ControlSnapshotManifest) error {
	root, err := filepath.Abs(snapshotRoot)
	if err != nil {
		return err
	}
	manifestDigest, _, err := hashFile(filepath.Join(root, "snapshot.json"))
	if err != nil {
		return err
	}
	writer := tar.NewWriter(destination)
	seenControls := map[string]bool{}
	var objectCount int
	var objectBytes int64
	err = filepath.WalkDir(root, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == root {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			relative, err := filepath.Rel(root, filePath)
			if err != nil {
				return err
			}
			directory := filepath.ToSlash(relative)
			if directory != "artifacts" && directory != "artifacts/objects" && directory != "artifacts/objects/sha256" && !strings.HasPrefix(directory, "artifacts/objects/sha256/") {
				return fmt.Errorf("snapshot contains an unexpected directory: %s", directory)
			}
			return nil
		}
		info, err := os.Lstat(filePath)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if !info.Mode().IsRegular() {
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("snapshot contains a symbolic link: %s", name)
			}
			return fmt.Errorf("snapshot contains an unsupported file type: %s", name)
		}
		expectedDigest := ""
		switch name {
		case "axiom.db":
			expectedDigest = manifest.DatabaseHash
			seenControls[name] = true
		case "master.key":
			expectedDigest = manifest.VaultKeyHash
			seenControls[name] = true
		case "snapshot.json":
			expectedDigest = manifestDigest
			seenControls[name] = true
		default:
			parts := strings.Split(name, "/")
			if len(parts) != 5 || parts[0] != "artifacts" || parts[1] != "objects" || parts[2] != "sha256" || !validDigest(parts[4]) || parts[3] != parts[4][:2] {
				return fmt.Errorf("snapshot contains an unexpected file: %s", name)
			}
			expectedDigest = parts[4]
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = name
		header.Typeflag = tar.TypeReg
		header.Mode = 0o600
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		file, err := os.Open(filePath)
		if err != nil {
			return err
		}
		hasher := sha256.New()
		copied, copyErr := io.Copy(io.MultiWriter(writer, hasher), contextReader{ctx: ctx, reader: file})
		closeErr := file.Close()
		if copyErr == nil && copied != info.Size() {
			copyErr = io.ErrUnexpectedEOF
		}
		if copyErr != nil || closeErr != nil {
			return errors.Join(copyErr, closeErr)
		}
		if hex.EncodeToString(hasher.Sum(nil)) != expectedDigest {
			return fmt.Errorf("snapshot file %s changed or failed content verification while encrypting", name)
		}
		if strings.HasPrefix(name, "artifacts/") {
			if objectBytes > int64(^uint64(0)>>1)-copied {
				return fmt.Errorf("snapshot object byte count overflow while encrypting")
			}
			objectCount++
			objectBytes += copied
		}
		return nil
	})
	if err == nil {
		if len(seenControls) != 3 || objectCount != manifest.ObjectCount || objectBytes != manifest.ObjectBytes {
			err = fmt.Errorf("snapshot contents changed while encrypting: controls=%d objects=%d bytes=%d", len(seenControls), objectCount, objectBytes)
		}
	}
	return errors.Join(err, writer.Close())
}

type backupFrameWriter struct {
	writer io.Writer
	aead   cipher.AEAD
	buffer []byte
	seq    uint64
	closed bool
}

func newBackupFrameWriter(writer io.Writer, aead cipher.AEAD) (*backupFrameWriter, error) {
	if err := writeBackupBytes(writer, []byte(backupHeader)); err != nil {
		return nil, err
	}
	return &backupFrameWriter{writer: writer, aead: aead, buffer: make([]byte, 0, backupFrameBytes)}, nil
}

func (w *backupFrameWriter) Write(data []byte) (int, error) {
	if w.closed {
		return 0, os.ErrClosed
	}
	total := len(data)
	for len(data) > 0 {
		space := backupFrameBytes - len(w.buffer)
		if space == 0 {
			if err := w.writeFrame(backupFrameData, w.buffer); err != nil {
				return total - len(data), err
			}
			w.buffer = w.buffer[:0]
			space = backupFrameBytes
		}
		n := len(data)
		if n > space {
			n = space
		}
		w.buffer = append(w.buffer, data[:n]...)
		data = data[n:]
	}
	return total, nil
}

func (w *backupFrameWriter) Close() error {
	if w.closed {
		return os.ErrClosed
	}
	w.closed = true
	if len(w.buffer) > 0 {
		if err := w.writeFrame(backupFrameData, w.buffer); err != nil {
			return err
		}
	}
	return w.writeFrame(backupFrameFinal, nil)
}

func (w *backupFrameWriter) writeFrame(flags byte, plaintext []byte) error {
	if len(plaintext) > backupFrameBytes {
		return fmt.Errorf("encrypted backup frame exceeds %d bytes", backupFrameBytes)
	}
	nonce := make([]byte, w.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	ciphertextLength := len(plaintext) + w.aead.Overhead()
	if err := writeBackupFrame(w.writer, w.aead, w.seq, flags, nonce, plaintext, uint32(ciphertextLength)); err != nil {
		return err
	}
	w.seq++
	return nil
}

func writeBackupFrame(writer io.Writer, aead cipher.AEAD, sequence uint64, flags byte, nonce, plaintext []byte, ciphertextLength uint32) error {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], ciphertextLength)
	aad := backupFrameAAD(sequence, flags, length[:])
	ciphertext := aead.Seal(nil, nonce, plaintext, aad)
	if len(ciphertext) != int(ciphertextLength) {
		return fmt.Errorf("unexpected encrypted backup frame size")
	}
	if err := writeBackupBytes(writer, []byte{flags}); err != nil {
		return err
	}
	if err := writeBackupBytes(writer, length[:]); err != nil {
		return err
	}
	if err := writeBackupBytes(writer, nonce); err != nil {
		return err
	}
	return writeBackupBytes(writer, ciphertext)
}

func writeBackupBytes(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(data) {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func backupFrameAAD(sequence uint64, flags byte, length []byte) []byte {
	aad := make([]byte, len(backupHeader)+8+1+len(length))
	copy(aad, backupHeader)
	binary.BigEndian.PutUint64(aad[len(backupHeader):], sequence)
	position := len(backupHeader) + 8
	aad[position] = flags
	copy(aad[position+1:], length)
	return aad
}

func extractEncryptedSnapshot(ctx context.Context, encryptedFile, destinationRoot string, key []byte) error {
	file, err := os.Open(encryptedFile)
	if err != nil {
		return err
	}
	defer file.Close()
	var header [len(backupHeader)]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return fmt.Errorf("read encrypted snapshot header: %w", err)
	}
	if string(header[:]) != backupHeader {
		return fmt.Errorf("unsupported encrypted snapshot format")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	frames := &backupFrameReader{ctx: ctx, reader: file, aead: aead, buffer: make([]byte, 0, backupFrameBytes)}
	reader := tar.NewReader(frames)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read encrypted snapshot archive: %w", err)
		}
		target, err := safeSnapshotArchivePath(destinationRoot, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 {
				return fmt.Errorf("encrypted snapshot contains a negative file size")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			copied, copyErr := io.CopyN(output, reader, header.Size)
			if copyErr == nil && copied != header.Size {
				copyErr = io.ErrUnexpectedEOF
			}
			if copyErr == nil {
				copyErr = output.Sync()
			}
			closeErr := output.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("encrypted snapshot contains unsupported archive entry type %d", header.Typeflag)
		}
	}
	var padding [32 * 1024]byte
	for {
		n, err := frames.Read(padding[:])
		for _, value := range padding[:n] {
			if value != 0 {
				return fmt.Errorf("encrypted snapshot contains data after the tar end marker")
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("authenticate encrypted snapshot trailer: %w", err)
		}
	}
	if !frames.finalSeen {
		return fmt.Errorf("encrypted snapshot is missing its authenticated final frame")
	}
	return nil
}

func safeSnapshotArchivePath(root, name string) (string, error) {
	if name == "" || strings.Contains(name, "\\") || strings.Contains(name, ":") || path.IsAbs(name) {
		return "", fmt.Errorf("encrypted snapshot contains an unsafe path")
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("encrypted snapshot contains an unsafe path")
	}
	target := filepath.Join(root, filepath.FromSlash(clean))
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("encrypted snapshot path escapes its destination")
	}
	return target, nil
}

type backupFrameReader struct {
	ctx       context.Context
	reader    io.Reader
	aead      cipher.AEAD
	buffer    []byte
	sequence  uint64
	finalSeen bool
	err       error
}

func (r *backupFrameReader) Read(output []byte) (int, error) {
	if len(output) == 0 {
		return 0, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	if err := r.ctx.Err(); err != nil {
		r.err = err
		return 0, r.err
	}
	if len(r.buffer) == 0 {
		if r.finalSeen {
			return 0, io.EOF
		}
		if err := r.readFrame(); err != nil {
			r.err = err
			return 0, err
		}
		if r.finalSeen {
			return 0, io.EOF
		}
	}
	n := copy(output, r.buffer)
	r.buffer = r.buffer[n:]
	return n, nil
}

func (r *backupFrameReader) readFrame() error {
	var flags [1]byte
	if _, err := io.ReadFull(r.reader, flags[:]); err != nil {
		return fmt.Errorf("encrypted snapshot ended before its final frame: %w", err)
	}
	var lengthBytes [4]byte
	if _, err := io.ReadFull(r.reader, lengthBytes[:]); err != nil {
		return fmt.Errorf("read encrypted snapshot frame length: %w", err)
	}
	length := binary.BigEndian.Uint32(lengthBytes[:])
	if length < uint32(r.aead.Overhead()) || length > backupFrameBytes+uint32(r.aead.Overhead()) {
		return fmt.Errorf("encrypted snapshot frame length is invalid")
	}
	var nonce [12]byte
	if _, err := io.ReadFull(r.reader, nonce[:]); err != nil {
		return fmt.Errorf("read encrypted snapshot frame nonce: %w", err)
	}
	ciphertext := make([]byte, int(length))
	if _, err := io.ReadFull(r.reader, ciphertext); err != nil {
		return fmt.Errorf("read encrypted snapshot frame: %w", err)
	}
	plaintext, err := r.aead.Open(nil, nonce[:], ciphertext, backupFrameAAD(r.sequence, flags[0], lengthBytes[:]))
	if err != nil {
		return fmt.Errorf("authenticate encrypted snapshot frame %d: %w", r.sequence, err)
	}
	r.sequence++
	switch flags[0] {
	case backupFrameData:
		if len(plaintext) == 0 {
			return fmt.Errorf("encrypted snapshot contains an empty data frame")
		}
		r.buffer = plaintext
	case backupFrameFinal:
		if len(plaintext) != 0 {
			return fmt.Errorf("encrypted snapshot final frame is not empty")
		}
		var extra [1]byte
		n, trailingErr := r.reader.Read(extra[:])
		if n != 0 || trailingErr == nil {
			return fmt.Errorf("encrypted snapshot has trailing bytes after its final frame")
		}
		if !errors.Is(trailingErr, io.EOF) {
			return fmt.Errorf("check encrypted snapshot trailer: %w", trailingErr)
		}
		r.finalSeen = true
	default:
		return fmt.Errorf("encrypted snapshot frame has unknown flags")
	}
	return nil
}
