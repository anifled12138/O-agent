//go:build linux

package projectquota

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

type Manager interface {
	Health(context.Context) (ProbeResult, error)
	Apply(context.Context, string, uint32, int64) (WorkspaceQuota, error)
	Inspect(context.Context, string, uint32) (WorkspaceQuota, error)
	Release(context.Context, string, uint32) (WorkspaceQuota, error)
}

type HelperClient struct{ socketPath string }

func NewHelperClient(socketPath string) (*HelperClient, error) {
	if socketPath == "" || !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath {
		return nil, errors.New("quota helper socket path must be a canonical absolute path")
	}
	return &HelperClient{socketPath: socketPath}, nil
}

func (client *HelperClient) Health(ctx context.Context) (ProbeResult, error) {
	response, err := client.requestResponse(ctx, quotaHelperRequest{Action: "health"})
	if err != nil {
		return ProbeResult{}, err
	}
	if response.Mount == nil || !response.Mount.MountSupported || !response.Mount.QuotaOptionFound || response.Mount.HardLimitVerified {
		return ProbeResult{}, errors.New("quota helper health did not verify mount prerequisites and correctly report unverified per-task enforcement")
	}
	return *response.Mount, nil
}

func (client *HelperClient) Apply(ctx context.Context, path string, projectID uint32, limitBytes int64) (WorkspaceQuota, error) {
	return client.request(ctx, quotaHelperRequest{Action: "apply", Path: path, ProjectID: projectID, Limit: limitBytes})
}

func (client *HelperClient) Inspect(ctx context.Context, path string, projectID uint32) (WorkspaceQuota, error) {
	return client.request(ctx, quotaHelperRequest{Action: "inspect", Path: path, ProjectID: projectID})
}

func (client *HelperClient) Release(ctx context.Context, path string, projectID uint32) (WorkspaceQuota, error) {
	return client.request(ctx, quotaHelperRequest{Action: "release", Path: path, ProjectID: projectID})
}

func (client *HelperClient) request(ctx context.Context, request quotaHelperRequest) (WorkspaceQuota, error) {
	response, err := client.requestResponse(ctx, request)
	if err != nil {
		return WorkspaceQuota{}, err
	}
	if response.Quota == nil || response.Quota.ProjectID != request.ProjectID {
		return WorkspaceQuota{}, errors.New("quota helper response did not read back the requested project ID")
	}
	if request.Action == "apply" && (!response.Quota.Applied || response.Quota.LimitBytes != request.Limit) {
		return WorkspaceQuota{}, errors.New("quota helper response did not verify the requested hard limit")
	}
	if request.Action == "release" && (response.Quota.Applied || response.Quota.LimitBytes != 0) {
		return WorkspaceQuota{}, errors.New("quota helper response did not verify quota release")
	}
	return *response.Quota, nil
}

func (client *HelperClient) requestResponse(ctx context.Context, request quotaHelperRequest) (quotaHelperResponse, error) {
	if client == nil || client.socketPath == "" || ctx == nil {
		return quotaHelperResponse{}, errors.New("quota helper request identity is invalid")
	}
	if request.Action != "health" && (!validHelperWorkspacePath(request.Path) || request.ProjectID == 0) {
		return quotaHelperResponse{}, errors.New("quota helper request identity is invalid")
	}
	if request.Action == "apply" && request.Limit <= 0 {
		return quotaHelperResponse{}, errors.New("quota helper apply request must set a positive limit")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return quotaHelperResponse{}, err
	}
	encoded = append(encoded, '\n')
	if len(encoded) > maxQuotaHelperRequestBytes {
		return quotaHelperResponse{}, errors.New("quota helper request exceeds the configured size limit")
	}
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "unix", client.socketPath)
	if err != nil {
		return quotaHelperResponse{}, fmt.Errorf("connect to root quota helper: %w", err)
	}
	defer connection.Close()
	deadline := time.Now().Add(15 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return quotaHelperResponse{}, err
	}
	if err := writeFull(connection, encoded); err != nil {
		return quotaHelperResponse{}, fmt.Errorf("send quota helper request: %w", err)
	}
	line, err := bufio.NewReaderSize(connection, maxQuotaHelperRequestBytes).ReadSlice('\n')
	if err != nil {
		return quotaHelperResponse{}, fmt.Errorf("read quota helper response: %w", err)
	}
	if len(line) > maxQuotaHelperRequestBytes {
		return quotaHelperResponse{}, errors.New("quota helper response exceeds the configured size limit")
	}
	var response quotaHelperResponse
	decoder := json.NewDecoder(bytes.NewReader(line[:len(line)-1]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return quotaHelperResponse{}, fmt.Errorf("decode quota helper response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return quotaHelperResponse{}, errors.New("quota helper response contains trailing data")
	}
	if response.Error != "" {
		return quotaHelperResponse{}, errors.New(response.Error)
	}
	return response, nil
}

func writeFull(writer io.Writer, payload []byte) error {
	for len(payload) > 0 {
		written, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if written == 0 {
			return os.ErrInvalid
		}
		payload = payload[written:]
	}
	return nil
}
