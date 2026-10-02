package execution

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"axiom.local/agent/internal/domain"
)

type nodeOutboxRecord struct {
	TaskID         string          `json:"taskId"`
	LeaseToken     string          `json:"leaseToken"`
	Result         json.RawMessage `json:"result"`
	UploadedResult json.RawMessage `json:"uploadedResult,omitempty"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
}

type nodeTaskOutbox struct {
	root string
	mu   sync.Mutex
}

func newNodeTaskOutbox(root string) (*nodeTaskOutbox, error) {
	root = strings.TrimSpace(root)
	if root == "" || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("node task outbox path must be absolute: %w", domain.ErrInvalid)
	}
	root = filepath.Clean(root)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create local node task outbox: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect local node task outbox: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("local node task outbox must be a real directory")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("restrict local node task outbox permissions: %w", err)
	}
	return &nodeTaskOutbox{root: root}, nil
}

func (o *nodeTaskOutbox) Save(taskID, leaseToken string, result json.RawMessage) (nodeOutboxRecord, error) {
	if o == nil || taskID == "" || leaseToken == "" || len(result) == 0 || !json.Valid(result) {
		return nodeOutboxRecord{}, fmt.Errorf("local outbox task identity, lease and valid result are required: %w", domain.ErrInvalid)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	inputResult := append(json.RawMessage(nil), result...)
	directory := o.taskDirectory(taskID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nodeOutboxRecord{}, fmt.Errorf("create task outbox entry: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nodeOutboxRecord{}, fmt.Errorf("inspect task outbox entry: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nodeOutboxRecord{}, errors.New("task outbox entry must be a real directory")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nodeOutboxRecord{}, fmt.Errorf("restrict task outbox permissions: %w", err)
	}
	result, err = o.preserveProjectDelta(directory, result)
	if err != nil {
		return nodeOutboxRecord{}, err
	}
	now := time.Now().UTC()
	record := nodeOutboxRecord{TaskID: taskID, LeaseToken: leaseToken, Result: result, CreatedAt: now, UpdatedAt: now}
	path := filepath.Join(directory, "record.json")
	if previous, readErr := readNodeOutboxRecord(path); readErr == nil {
		if previous.TaskID != taskID || previous.LeaseToken != leaseToken || !bytes.Equal(previous.Result, result) {
			return nodeOutboxRecord{}, fmt.Errorf("task already has a different durable outbox result: %w", domain.ErrConflict)
		}
		return previous, nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return nodeOutboxRecord{}, fmt.Errorf("read existing task outbox record: %w", readErr)
	}
	if err := writeNodeOutboxRecord(path, record); err != nil {
		return nodeOutboxRecord{}, err
	}
	readBack, err := readNodeOutboxRecord(path)
	if err != nil {
		return nodeOutboxRecord{}, fmt.Errorf("verify durable task outbox record: %w", err)
	}
	if readBack.TaskID != taskID || readBack.LeaseToken != leaseToken || !bytes.Equal(readBack.Result, result) {
		return nodeOutboxRecord{}, domain.ErrConflict
	}
	var original, persisted localTranscriptEnvelope
	if json.Unmarshal(inputResult, &original) == nil && json.Unmarshal(readBack.Result, &persisted) == nil {
		for _, pair := range [][2]string{{original.ProjectDeltaPath, persisted.ProjectDeltaPath}, {original.ProjectLFSPath, persisted.ProjectLFSPath}, {original.ProjectSubmoduleDeltasPath, persisted.ProjectSubmoduleDeltasPath}} {
			if pair[0] == "" || filepath.Clean(pair[0]) == filepath.Clean(pair[1]) {
				continue
			}
			if err := os.Remove(pair[0]); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nodeOutboxRecord{}, fmt.Errorf("remove original project artifact after durable outbox copy: %w", err)
			}
			if _, err := os.Lstat(pair[0]); !errors.Is(err, os.ErrNotExist) {
				if err == nil {
					err = errors.New("original project artifact remains after cleanup")
				}
				return nodeOutboxRecord{}, fmt.Errorf("verify original project artifact cleanup: %w", err)
			}
		}
	}
	return readBack, nil
}

func (o *nodeTaskOutbox) SaveUploadedResult(record nodeOutboxRecord, uploaded json.RawMessage) (nodeOutboxRecord, error) {
	if o == nil || record.TaskID == "" || len(uploaded) == 0 || !json.Valid(uploaded) {
		return nodeOutboxRecord{}, fmt.Errorf("verified uploaded result is required: %w", domain.ErrInvalid)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	path := filepath.Join(o.taskDirectory(record.TaskID), "record.json")
	persisted, err := readNodeOutboxRecord(path)
	if err != nil {
		return nodeOutboxRecord{}, fmt.Errorf("read task outbox before recording cloud result: %w", err)
	}
	if persisted.TaskID != record.TaskID || persisted.LeaseToken != record.LeaseToken || !bytes.Equal(persisted.Result, record.Result) {
		return nodeOutboxRecord{}, domain.ErrConflict
	}
	if len(persisted.UploadedResult) > 0 && !bytes.Equal(persisted.UploadedResult, uploaded) {
		return nodeOutboxRecord{}, domain.ErrConflict
	}
	persisted.UploadedResult = append(json.RawMessage(nil), uploaded...)
	persisted.UpdatedAt = time.Now().UTC()
	if err := writeNodeOutboxRecord(path, persisted); err != nil {
		return nodeOutboxRecord{}, err
	}
	readBack, err := readNodeOutboxRecord(path)
	if err != nil || !bytes.Equal(readBack.UploadedResult, uploaded) {
		return nodeOutboxRecord{}, errors.Join(fmt.Errorf("verify persisted cloud artifact result: %w", domain.ErrConflict), err)
	}
	return readBack, nil
}

func (o *nodeTaskOutbox) List() ([]nodeOutboxRecord, error) {
	if o == nil {
		return nil, nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	rootInfo, err := os.Lstat(o.root)
	if err != nil {
		return nil, fmt.Errorf("inspect local node task outbox before recovery: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("local node task outbox root is not a real directory")
	}
	entries, err := os.ReadDir(o.root)
	if err != nil {
		return nil, fmt.Errorf("list local node task outbox: %w", err)
	}
	result := make([]nodeOutboxRecord, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			return nil, fmt.Errorf("unexpected file in local node task outbox: %s", entry.Name())
		}
		info, err := os.Lstat(filepath.Join(o.root, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("inspect local outbox entry %s: %w", entry.Name(), err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("local outbox entry %s is not a real directory", entry.Name())
		}
		record, err := readNodeOutboxRecord(filepath.Join(o.root, entry.Name(), "record.json"))
		if errors.Is(err, os.ErrNotExist) {
			if err := o.removeIncompleteEntry(entry.Name()); err != nil {
				return nil, fmt.Errorf("recover incomplete local node outbox entry %s: %w", entry.Name(), err)
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read local outbox entry %s: %w", entry.Name(), err)
		}
		if entry.Name() != o.taskKey(record.TaskID) {
			return nil, fmt.Errorf("local outbox entry identity does not match its directory: %w", domain.ErrConflict)
		}
		result = append(result, record)
	}
	return result, nil
}

func (o *nodeTaskOutbox) removeIncompleteEntry(name string) error {
	if len(name) != sha256.Size*2 {
		return errors.New("incomplete outbox directory has an invalid task key")
	}
	if _, err := hex.DecodeString(name); err != nil {
		return errors.New("incomplete outbox directory has an invalid task key")
	}
	directory := filepath.Join(o.root, name)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "project-delta.bundle":
			info, err := os.Lstat(filepath.Join(directory, entry.Name()))
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("incomplete project delta is not a regular file")
			}
		case "project-lfs-objects.tar":
			info, err := os.Lstat(filepath.Join(directory, entry.Name()))
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("incomplete project Git LFS archive is not a regular file")
			}
		default:
			if !strings.HasPrefix(entry.Name(), ".project-delta-") && !strings.HasPrefix(entry.Name(), ".record-") {
				return fmt.Errorf("unexpected file in incomplete outbox entry: %s", entry.Name())
			}
			info, err := os.Lstat(filepath.Join(directory, entry.Name()))
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("incomplete outbox temporary is not a regular file")
			}
		}
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = errors.New("incomplete outbox entry remains after cleanup")
		}
		return err
	}
	return syncNodeOutboxDirectory(o.root)
}

func (o *nodeTaskOutbox) Remove(taskID string) error {
	if o == nil || taskID == "" {
		return domain.ErrInvalid
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	directory := o.taskDirectory(taskID)
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("remove verified local node outbox entry: %w", err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = errors.New("local outbox entry remains after removal")
		}
		return fmt.Errorf("verify local node outbox cleanup: %w", err)
	}
	if err := syncNodeOutboxDirectory(o.root); err != nil {
		return fmt.Errorf("persist local node outbox cleanup: %w", err)
	}
	return nil
}

func (o *nodeTaskOutbox) taskDirectory(taskID string) string {
	return filepath.Join(o.root, o.taskKey(taskID))
}

func (o *nodeTaskOutbox) taskKey(taskID string) string {
	digest := sha256.Sum256([]byte(taskID))
	return hex.EncodeToString(digest[:])
}

func (o *nodeTaskOutbox) preserveProjectDelta(directory string, result json.RawMessage) (json.RawMessage, error) {
	var envelope localTranscriptEnvelope
	if err := json.Unmarshal(result, &envelope); err != nil || envelope.ProjectDeltaPath == "" && envelope.ProjectLFSPath == "" && envelope.ProjectSubmoduleDeltasPath == "" {
		return result, nil
	}
	paths := []struct {
		name   string
		source string
		set    func(string)
	}{
		{"project-delta.bundle", envelope.ProjectDeltaPath, func(value string) { envelope.ProjectDeltaPath = value }},
		{"project-lfs-objects.tar", envelope.ProjectLFSPath, func(value string) { envelope.ProjectLFSPath = value }},
		{"project-submodule-deltas.tar", envelope.ProjectSubmoduleDeltasPath, func(value string) { envelope.ProjectSubmoduleDeltasPath = value }},
	}
	for _, item := range paths {
		if item.source == "" {
			continue
		}
		source := filepath.Clean(item.source)
		if !filepath.IsAbs(source) {
			return nil, fmt.Errorf("local project artifact path must be absolute before outbox persistence: %w", domain.ErrInvalid)
		}
		info, err := os.Lstat(source)
		if err != nil {
			return nil, fmt.Errorf("inspect local project artifact before outbox persistence: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 {
			return nil, fmt.Errorf("local project artifact must be a non-empty regular file before outbox persistence: %w", domain.ErrInvalid)
		}
		destination := filepath.Join(directory, item.name)
		if filepath.Clean(source) != filepath.Clean(destination) {
			if err := copyNodeOutboxFile(source, destination, info.Size()); err != nil {
				return nil, err
			}
		}
		item.set(destination)
	}
	updated, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode local result with durable project delta path: %w", err)
	}
	return updated, nil
}

func copyNodeOutboxFile(source, destination string, expectedSize int64) (returnErr error) {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open local project delta for outbox: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".project-delta-*")
	if err != nil {
		return errors.Join(fmt.Errorf("create durable project delta copy: %w", err), input.Close())
	}
	tmpPath := tmp.Name()
	defer func() {
		if err := os.Remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove temporary durable project delta copy: %w", err))
		}
	}()
	written, copyErr := io.Copy(tmp, input)
	inputCloseErr := input.Close()
	if copyErr != nil {
		closeErr := tmp.Close()
		return errors.Join(fmt.Errorf("copy project delta into local outbox: %w", copyErr), closeErr, inputCloseErr)
	}
	if written != expectedSize {
		closeErr := tmp.Close()
		return errors.Join(fmt.Errorf("project delta changed while it was copied into the outbox: %w", domain.ErrConflict), closeErr, inputCloseErr)
	}
	if inputCloseErr != nil {
		return errors.Join(fmt.Errorf("close project delta source after outbox copy: %w", inputCloseErr), tmp.Close())
	}
	if err := tmp.Sync(); err != nil {
		closeErr := tmp.Close()
		return errors.Join(fmt.Errorf("sync durable project delta copy: %w", err), closeErr)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("restrict durable project delta permissions: %w", err)
	}
	if err := os.Rename(tmpPath, destination); err != nil {
		return fmt.Errorf("publish durable project delta into local outbox: %w", err)
	}
	return syncNodeOutboxDirectory(filepath.Dir(destination))
}

func readNodeOutboxRecord(path string) (nodeOutboxRecord, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nodeOutboxRecord{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nodeOutboxRecord{}, errors.New("outbox record must be a regular file")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nodeOutboxRecord{}, err
	}
	var record nodeOutboxRecord
	if err := json.Unmarshal(content, &record); err != nil {
		return nodeOutboxRecord{}, fmt.Errorf("decode local node outbox record: %w", err)
	}
	if record.TaskID == "" || record.LeaseToken == "" || len(record.Result) == 0 || !json.Valid(record.Result) || record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() || (len(record.UploadedResult) > 0 && !json.Valid(record.UploadedResult)) {
		return nodeOutboxRecord{}, fmt.Errorf("local node outbox record is incomplete: %w", domain.ErrConflict)
	}
	return record, nil
}

func writeNodeOutboxRecord(path string, record nodeOutboxRecord) (returnErr error) {
	content, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode local node outbox record: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".record-*")
	if err != nil {
		return fmt.Errorf("create temporary local node outbox record: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err := os.Remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove temporary local node outbox record: %w", err))
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		closeErr := tmp.Close()
		return errors.Join(fmt.Errorf("write local node outbox record: %w", err), closeErr)
	}
	if err := tmp.Sync(); err != nil {
		closeErr := tmp.Close()
		return errors.Join(fmt.Errorf("sync local node outbox record: %w", err), closeErr)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("restrict local node outbox record permissions: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publish durable local node outbox record: %w", err)
	}
	return syncNodeOutboxDirectory(filepath.Dir(path))
}

func syncNodeOutboxDirectory(path string) error {
	if os.PathSeparator == '\\' {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

func (r nodeOutboxRecord) hasUploadedResult() bool {
	return len(r.UploadedResult) > 0 && json.Valid(r.UploadedResult)
}
