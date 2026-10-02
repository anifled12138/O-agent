package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"axiom.local/agent/internal/artifactstore"
	"axiom.local/agent/internal/storage"
)

func TestArtifactHTTPTransferReadsBackVerifiedObject(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	artifactService, err := artifactstore.New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{workspaceID: owner, store: store, artifacts: artifactService}
	handler := server.Handler()
	content := []byte("web bridge has no compute; artifact content is verified at rest")
	digest := sha256.Sum256(content)
	digestText := hex.EncodeToString(digest[:])
	beginBody, _ := json.Marshal(map[string]any{"fileName": "output.txt", "mediaType": "text/plain", "expectedSize": len(content), "expectedSha256": digestText})
	beginRequest := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/uploads", bytes.NewReader(beginBody))
	beginRequest.Header.Set("Idempotency-Key", "artifact-http-test")
	beginResponse := httptest.NewRecorder()
	handler.ServeHTTP(beginResponse, beginRequest)
	var begun struct {
		Upload storage.ArtifactUpload `json:"upload"`
	}
	if beginResponse.Code != http.StatusCreated || json.Unmarshal(beginResponse.Body.Bytes(), &begun) != nil || begun.Upload.ID == "" {
		t.Fatalf("begin upload failed: %d %s", beginResponse.Code, beginResponse.Body.String())
	}
	chunkDigest := sha256.Sum256(content)
	chunkRequest := httptest.NewRequest(http.MethodPut, "/api/v1/artifacts/uploads/"+begun.Upload.ID+"/chunks/0", bytes.NewReader(content))
	chunkRequest.Header.Set("X-Chunk-SHA256", hex.EncodeToString(chunkDigest[:]))
	chunkResponse := httptest.NewRecorder()
	handler.ServeHTTP(chunkResponse, chunkRequest)
	if chunkResponse.Code != http.StatusOK {
		t.Fatalf("upload chunk failed: %d %s", chunkResponse.Code, chunkResponse.Body.String())
	}
	finalizeRequest := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/uploads/"+begun.Upload.ID+"/finalize", nil)
	finalizeResponse := httptest.NewRecorder()
	handler.ServeHTTP(finalizeResponse, finalizeRequest)
	var artifact storage.Artifact
	if finalizeResponse.Code != http.StatusOK || json.Unmarshal(finalizeResponse.Body.Bytes(), &artifact) != nil || artifact.SHA256 != digestText {
		t.Fatalf("finalize failed: %d %s", finalizeResponse.Code, finalizeResponse.Body.String())
	}
	capacityRequest := httptest.NewRequest(http.MethodGet, "/api/v1/system/artifact-storage", nil)
	capacityResponse := httptest.NewRecorder()
	handler.ServeHTTP(capacityResponse, capacityRequest)
	var capacity artifactstore.Capacity
	if capacityResponse.Code != http.StatusOK || json.Unmarshal(capacityResponse.Body.Bytes(), &capacity) != nil || capacity.ObjectBytes != int64(len(content)) || capacity.MetadataDatabaseBytes == 0 || !capacity.FilesystemMeasured || capacity.FilesystemFreeBytes > capacity.FilesystemTotalBytes || capacity.ObservedAt.IsZero() {
		t.Fatalf("artifact storage capacity did not read back durable object state: status=%d body=%s capacity=%+v", capacityResponse.Code, capacityResponse.Body.String(), capacity)
	}
	downloadRequest := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/"+artifact.ID, nil)
	downloadResponse := httptest.NewRecorder()
	handler.ServeHTTP(downloadResponse, downloadRequest)
	if downloadResponse.Code != http.StatusOK || !bytes.Equal(downloadResponse.Body.Bytes(), content) || downloadResponse.Header().Get("X-Artifact-SHA256") != digestText || downloadResponse.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("download did not return the verified object: status=%d headers=%v body=%q", downloadResponse.Code, downloadResponse.Header(), downloadResponse.Body.Bytes())
	}
}

func TestS3ArtifactBrowserDownloadRedirectsToShortLivedObjectURL(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var object []byte
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read failed", http.StatusBadRequest)
				return
			}
			mu.Lock()
			object = body
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			mu.Lock()
			size := len(object)
			mu.Unlock()
			w.Header().Set("Content-Length", strconv.Itoa(size))
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			mu.Lock()
			body := append([]byte(nil), object...)
			mu.Unlock()
			if disposition := r.URL.Query().Get("response-content-disposition"); disposition != "" {
				w.Header().Set("Content-Disposition", disposition)
			}
			w.Header().Set("Cache-Control", r.URL.Query().Get("response-cache-control"))
			w.Header().Set("Content-Type", r.URL.Query().Get("response-content-type"))
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write(body)
		default:
			http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		}
	}))
	defer provider.Close()
	artifactService, err := artifactstore.NewWithS3(dataDir, store, artifactstore.S3Config{Endpoint: provider.URL, Region: "us-east-1", Bucket: "bucket", AccessKeyID: "test-access", SecretAccessKey: "test-secret", Prefix: "o-agent/test", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("the browser should fetch this object from S3, not through O")
	digest := sha256.Sum256(content)
	artifact, err := artifactService.StoreFromReader(ctx, owner, "report final.txt", "text/plain", "browser-direct-download", int64(len(content)), hex.EncodeToString(digest[:]), bytes.NewReader(content), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	handler := (&Server{workspaceID: owner, store: store, artifacts: artifactService}).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/"+url.PathEscape(artifact.ID), nil))
	if response.Code != http.StatusTemporaryRedirect || strings.Contains(response.Body.String(), string(content)) || response.Header().Get("Cache-Control") != "private, no-store" || response.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("browser download was not redirected without proxying object bytes and with private no-referrer policy: status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil || location.Host != strings.TrimPrefix(provider.URL, "http://") || location.Query().Get("X-Amz-Expires") != "300" || !strings.Contains(location.Query().Get("response-content-disposition"), "report%20final.txt") {
		t.Fatalf("browser redirect was not a five-minute, attachment-scoped object URL: location=%q err=%v", response.Header().Get("Location"), err)
	}
	download, err := http.Get(location.String())
	if err != nil {
		t.Fatal(err)
	}
	readBack, readErr := io.ReadAll(download.Body)
	closeErr := download.Body.Close()
	if download.StatusCode != http.StatusOK || readErr != nil || closeErr != nil || !bytes.Equal(readBack, content) || download.Header.Get("Content-Disposition") != "attachment; filename*=UTF-8''report%20final.txt" || download.Header.Get("Cache-Control") != "private, no-store" || download.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("direct provider download did not return the persisted private attachment: status=%d headers=%v bytes=%q read=%v close=%v", download.StatusCode, download.Header, readBack, readErr, closeErr)
	}
}

func TestBrowserAndNodeArtifactsUseChecksumBoundDirectS3UploadAndReadBack(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	objects := map[string][]byte{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/bucket/")
		etag := func(body []byte) string {
			digest := sha256.Sum256(body)
			return `"` + hex.EncodeToString(digest[:]) + `"`
		}
		switch {
		case r.Method == http.MethodPut && r.Header.Get("x-amz-copy-source") != "":
			source := strings.TrimPrefix(r.Header.Get("x-amz-copy-source"), "/bucket/")
			mu.Lock()
			body, found := objects[source]
			if !found || etag(body) != r.Header.Get("x-amz-copy-source-if-match") {
				mu.Unlock()
				http.Error(w, "copy source mismatch", http.StatusPreconditionFailed)
				return
			}
			objects[key] = append([]byte(nil), body...)
			mu.Unlock()
			_, _ = w.Write([]byte("<CopyObjectResult><ETag>copied</ETag></CopyObjectResult>"))
		case r.Method == http.MethodPut:
			body, readErr := io.ReadAll(r.Body)
			digest := sha256.Sum256(body)
			if readErr != nil || base64.StdEncoding.EncodeToString(digest[:]) != r.Header.Get("x-amz-checksum-sha256") {
				http.Error(w, "checksum mismatch", http.StatusBadRequest)
				return
			}
			mu.Lock()
			objects[key] = body
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodHead:
			mu.Lock()
			body, found := objects[key]
			body = append([]byte(nil), body...)
			mu.Unlock()
			if !found {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.Header().Set("ETag", etag(body))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet:
			mu.Lock()
			body, found := objects[key]
			body = append([]byte(nil), body...)
			mu.Unlock()
			if !found {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(body)
		case r.Method == http.MethodDelete:
			mu.Lock()
			delete(objects, key)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unsupported provider request", http.StatusMethodNotAllowed)
		}
	}))
	defer provider.Close()
	artifactService, err := artifactstore.NewWithS3(dataDir, store, artifactstore.S3Config{Endpoint: provider.URL, Region: "us-east-1", Bucket: "bucket", AccessKeyID: "test-access", SecretAccessKey: "test-secret", Prefix: "o-agent/test", PathStyle: true, EnableDirectUpload: true})
	if err != nil {
		t.Fatal(err)
	}
	const nodeCredential = "direct-upload-node-credential"
	const lease = "direct-upload-task-lease"
	node, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_direct_upload", UserID: owner, Name: "direct upload node", Platform: "linux"}, hashToken(nodeCredential))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, hashToken(nodeCredential), "linux", []string{"agent-runtime"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: "task_direct_upload", UserID: owner, NodeID: node.ID, IdempotencyKey: "direct-upload-task", Payload: json.RawMessage(`{"kind":"agent_prompt"}`)}, false, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExecutionTask(ctx, node.ID, hashToken(lease), time.Now().UTC().Add(time.Minute), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	server := &Server{workspaceID: owner, store: store, artifacts: artifactService}
	server.SetRemoteAuthentication("01234567890123456789012345678901")
	const browserSession = "browser-direct-upload-session"
	if err := store.CreateAuthSession(ctx, hashToken(browserSession), owner, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	content := []byte("task artifact directly uploaded to S3")
	digest := sha256.Sum256(content)
	beginBody, _ := json.Marshal(map[string]any{"fileName": "result.bin", "mediaType": "application/octet-stream", "expectedSize": len(content), "expectedSha256": hex.EncodeToString(digest[:])})
	unleasedRequest := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/tasks/task_direct_upload/artifacts/uploads", bytes.NewReader(beginBody))
	unleasedRequest.Header.Set("Authorization", "Bearer "+nodeCredential)
	unleasedRequest.Header.Set("X-O-Task-Lease", "wrong-or-expired-lease")
	unleasedRequest.Header.Set("Idempotency-Key", "task-result")
	unleasedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unleasedResponse, unleasedRequest)
	if unleasedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("node without the active task lease received an artifact upload capability: status=%d body=%s", unleasedResponse.Code, unleasedResponse.Body.String())
	}
	beginRequest := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/tasks/task_direct_upload/artifacts/uploads", bytes.NewReader(beginBody))
	beginRequest.Header.Set("Authorization", "Bearer "+nodeCredential)
	beginRequest.Header.Set("X-O-Task-Lease", lease)
	beginRequest.Header.Set("Idempotency-Key", "task-result")
	beginResponse := httptest.NewRecorder()
	handler.ServeHTTP(beginResponse, beginRequest)
	var begun struct {
		Upload         storage.ArtifactUpload `json:"upload"`
		DirectUpload   bool                   `json:"directUpload"`
		UploadURL      string                 `json:"uploadURL"`
		RequiredHeader map[string]string      `json:"requiredHeaders"`
	}
	if beginResponse.Code != http.StatusCreated || json.Unmarshal(beginResponse.Body.Bytes(), &begun) != nil || begun.Upload.ID == "" || !begun.DirectUpload || !begun.Upload.DirectUpload || begun.UploadURL == "" || begun.RequiredHeader["x-amz-checksum-sha256"] == "" || beginResponse.Header().Get("Cache-Control") != "private, no-store" || beginResponse.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("node did not receive the persisted direct-upload capability: status=%d body=%s result=%+v", beginResponse.Code, beginResponse.Body.String(), begun)
	}
	request, err := http.NewRequest(http.MethodPut, begun.UploadURL, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range begun.RequiredHeader {
		request.Header.Set(name, value)
	}
	providerResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	closeErr := providerResponse.Body.Close()
	if providerResponse.StatusCode != http.StatusOK || closeErr != nil {
		t.Fatalf("provider did not accept direct node upload: status=%d close=%v", providerResponse.StatusCode, closeErr)
	}
	finalizeBody, _ := json.Marshal(map[string]string{"role": "result_bundle"})
	finalizeRequest := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/tasks/task_direct_upload/artifacts/uploads/"+begun.Upload.ID+"/finalize", bytes.NewReader(finalizeBody))
	finalizeRequest.Header.Set("Authorization", "Bearer "+nodeCredential)
	finalizeRequest.Header.Set("X-O-Task-Lease", lease)
	finalizeResponse := httptest.NewRecorder()
	handler.ServeHTTP(finalizeResponse, finalizeRequest)
	var attached storage.ExecutionTaskArtifact
	if finalizeResponse.Code != http.StatusOK || json.Unmarshal(finalizeResponse.Body.Bytes(), &attached) != nil || attached.Role != "result_bundle" || attached.SHA256 != hex.EncodeToString(digest[:]) || attached.ByteSize != int64(len(content)) {
		t.Fatalf("artifact was not hash-verified and attached after direct upload: status=%d body=%s attached=%+v", finalizeResponse.Code, finalizeResponse.Body.String(), attached)
	}
	items, err := store.ExecutionTaskArtifacts(ctx, owner, "task_direct_upload")
	if err != nil || len(items) != 1 || items[0].ID != attached.ID || items[0].Role != "result_bundle" {
		t.Fatalf("direct-upload artifact association did not read back from storage: items=%+v err=%v", items, err)
	}

	webContent := []byte("browser uploads go straight to object storage")
	webDigest := sha256.Sum256(webContent)
	webBody, _ := json.Marshal(map[string]any{"fileName": "browser-result.bin", "mediaType": "application/octet-stream", "expectedSize": len(webContent), "expectedSha256": hex.EncodeToString(webDigest[:])})
	webUnauthenticated := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/uploads", bytes.NewReader(webBody))
	webUnauthenticated.Header.Set("Idempotency-Key", "browser-direct-upload-unauthenticated")
	webUnauthenticatedResponse := httptest.NewRecorder()
	handler.ServeHTTP(webUnauthenticatedResponse, webUnauthenticated)
	if webUnauthenticatedResponse.Code != http.StatusUnauthorized || strings.Contains(webUnauthenticatedResponse.Body.String(), "X-Amz-") {
		t.Fatalf("unauthenticated browser received an upload capability: status=%d body=%s", webUnauthenticatedResponse.Code, webUnauthenticatedResponse.Body.String())
	}
	if activeUploads, err := store.ActiveArtifactUploadCount(ctx); err != nil || activeUploads != 0 {
		t.Fatalf("unauthenticated browser created a durable upload record: active=%d err=%v", activeUploads, err)
	}
	webBegin := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/uploads", bytes.NewReader(webBody))
	webBegin.AddCookie(&http.Cookie{Name: authCookieName, Value: browserSession})
	webBegin.Header.Set("Idempotency-Key", "browser-direct-upload-1")
	webBeginResponse := httptest.NewRecorder()
	handler.ServeHTTP(webBeginResponse, webBegin)
	var webUpload struct {
		Upload         storage.ArtifactUpload `json:"upload"`
		DirectUpload   bool                   `json:"directUpload"`
		UploadURL      string                 `json:"uploadURL"`
		RequiredHeader map[string]string      `json:"requiredHeaders"`
	}
	if webBeginResponse.Code != http.StatusCreated || json.Unmarshal(webBeginResponse.Body.Bytes(), &webUpload) != nil || webUpload.Upload.ID == "" || !webUpload.DirectUpload || !webUpload.Upload.DirectUpload || webUpload.UploadURL == "" || webUpload.RequiredHeader["x-amz-checksum-sha256"] == "" || webBeginResponse.Header().Get("Cache-Control") != "private, no-store" || webBeginResponse.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("authenticated browser did not receive its persisted checksum-bound capability: status=%d body=%s result=%+v", webBeginResponse.Code, webBeginResponse.Body.String(), webUpload)
	}
	webRetry := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/uploads", bytes.NewReader(webBody))
	webRetry.AddCookie(&http.Cookie{Name: authCookieName, Value: browserSession})
	webRetry.Header.Set("Idempotency-Key", "browser-direct-upload-1")
	webRetryResponse := httptest.NewRecorder()
	handler.ServeHTTP(webRetryResponse, webRetry)
	var webRetryUpload struct {
		Upload       storage.ArtifactUpload `json:"upload"`
		Created      bool                   `json:"created"`
		DirectUpload bool                   `json:"directUpload"`
		UploadURL    string                 `json:"uploadURL"`
	}
	if webRetryResponse.Code != http.StatusOK || json.Unmarshal(webRetryResponse.Body.Bytes(), &webRetryUpload) != nil || webRetryUpload.Created || !webRetryUpload.DirectUpload || !webRetryUpload.Upload.DirectUpload || webRetryUpload.Upload.ID != webUpload.Upload.ID || webRetryUpload.UploadURL == "" || webRetryResponse.Header().Get("Cache-Control") != "private, no-store" || webRetryResponse.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("browser retry did not recover a fresh URL for the same durable direct upload: status=%d body=%s result=%+v", webRetryResponse.Code, webRetryResponse.Body.String(), webRetryUpload)
	}
	webPut, err := http.NewRequest(http.MethodPut, webRetryUpload.UploadURL, bytes.NewReader(webContent))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range webUpload.RequiredHeader {
		webPut.Header.Set(name, value)
	}
	webProviderResponse, err := http.DefaultClient.Do(webPut)
	if err != nil {
		t.Fatal(err)
	}
	webProviderCloseErr := webProviderResponse.Body.Close()
	if webProviderResponse.StatusCode != http.StatusOK || webProviderCloseErr != nil {
		t.Fatalf("object provider rejected the browser direct upload: status=%d close=%v", webProviderResponse.StatusCode, webProviderCloseErr)
	}
	webFinalize := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/uploads/"+webUpload.Upload.ID+"/finalize", nil)
	webFinalize.AddCookie(&http.Cookie{Name: authCookieName, Value: browserSession})
	webFinalizeResponse := httptest.NewRecorder()
	handler.ServeHTTP(webFinalizeResponse, webFinalize)
	var webArtifact storage.Artifact
	if webFinalizeResponse.Code != http.StatusOK || json.Unmarshal(webFinalizeResponse.Body.Bytes(), &webArtifact) != nil || webArtifact.SHA256 != hex.EncodeToString(webDigest[:]) || webArtifact.ByteSize != int64(len(webContent)) {
		t.Fatalf("browser direct upload was not verified and finalized from S3: status=%d body=%s artifact=%+v", webFinalizeResponse.Code, webFinalizeResponse.Body.String(), webArtifact)
	}
	largeWebBody, _ := json.Marshal(map[string]any{"fileName": "large-browser-result.bin", "mediaType": "application/octet-stream", "expectedSize": artifactstore.DirectUploadBatchMaxBytes + 1, "expectedSha256": strings.Repeat("a", 64)})
	largeWebBegin := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/uploads", bytes.NewReader(largeWebBody))
	largeWebBegin.AddCookie(&http.Cookie{Name: authCookieName, Value: browserSession})
	largeWebBegin.Header.Set("Idempotency-Key", "browser-large-chunked-upload")
	largeWebResponse := httptest.NewRecorder()
	handler.ServeHTTP(largeWebResponse, largeWebBegin)
	var largeWebUpload struct {
		Upload       storage.ArtifactUpload `json:"upload"`
		DirectUpload bool                   `json:"directUpload"`
		ChunkSize    int64                  `json:"chunkSize"`
		UploadURL    string                 `json:"uploadURL"`
	}
	if largeWebResponse.Code != http.StatusCreated || json.Unmarshal(largeWebResponse.Body.Bytes(), &largeWebUpload) != nil || largeWebUpload.Upload.ID == "" || largeWebUpload.Upload.DirectUpload || largeWebUpload.DirectUpload || largeWebUpload.ChunkSize != artifactstore.ChunkSize || largeWebUpload.UploadURL != "" || largeWebUpload.Upload.ExpectedSize != artifactstore.DirectUploadBatchMaxBytes+1 {
		t.Fatalf("browser artifact above the direct-batch threshold was rejected or not left on resumable chunks: status=%d body=%s upload=%+v", largeWebResponse.Code, largeWebResponse.Body.String(), largeWebUpload)
	}
}

func TestArtifactShareLinkRequiresOwnerSessionAndIsRevocable(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	artifactService, err := artifactstore.New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("shareable task result with durable permissions")
	digest := sha256.Sum256(content)
	digestText := hex.EncodeToString(digest[:])
	artifact, err := artifactService.StoreFromReader(ctx, owner, "result.txt", "text/plain", "share-link-http-test", int64(len(content)), digestText, bytes.NewReader(content), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{workspaceID: owner, store: store, artifacts: artifactService}
	server.SetRemoteAuthentication("01234567890123456789012345678901")
	handler := server.Handler()
	session := "owner-session-for-share-link-test"
	if err := store.CreateAuthSession(ctx, hashToken(session), owner, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	createURL := "/api/v1/artifacts/" + artifact.ID + "/share-links"
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, createURL, bytes.NewReader([]byte(`{}`))))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous caller created an artifact link: %d %s", unauthorized.Code, unauthorized.Body.String())
	}
	createRequest := httptest.NewRequest(http.MethodPost, createURL, bytes.NewReader([]byte(`{"expiresInSeconds":3600}`)))
	createRequest.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
	createResponse := httptest.NewRecorder()
	handler.ServeHTTP(createResponse, createRequest)
	var created artifactShareLinkResponse
	if createResponse.Code != http.StatusCreated || json.Unmarshal(createResponse.Body.Bytes(), &created) != nil || created.Link.ID == "" || created.Link.ArtifactID != artifact.ID || created.Token == "" || created.Path == "" {
		t.Fatalf("share-link creation did not return durable metadata and a one-time token: %d %s", createResponse.Code, createResponse.Body.String())
	}
	unauthorizedList := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedList, httptest.NewRequest(http.MethodGet, "/api/v1/artifact-share-links?artifactId="+artifact.ID, nil))
	if unauthorizedList.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous caller listed an owner's share links: %d", unauthorizedList.Code)
	}
	unauthorizedRevoke := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedRevoke, httptest.NewRequest(http.MethodDelete, "/api/v1/artifact-share-links/"+created.Link.ID, nil))
	if unauthorizedRevoke.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous caller revoked an owner's share link: %d", unauthorizedRevoke.Code)
	}
	sharedDownload := httptest.NewRecorder()
	handler.ServeHTTP(sharedDownload, httptest.NewRequest(http.MethodGet, created.Path, nil))
	if sharedDownload.Code != http.StatusOK || !bytes.Equal(sharedDownload.Body.Bytes(), content) || sharedDownload.Header().Get("X-Artifact-SHA256") != digestText || sharedDownload.Header().Get("Referrer-Policy") != "no-referrer" || sharedDownload.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("bearer link did not serve verified bytes with privacy headers: status=%d headers=%v body=%q", sharedDownload.Code, sharedDownload.Header(), sharedDownload.Body.Bytes())
	}
	wrongArtifact := httptest.NewRecorder()
	handler.ServeHTTP(wrongArtifact, httptest.NewRequest(http.MethodGet, "/api/v1/shared/artifacts/another-artifact?token="+created.Token, nil))
	if wrongArtifact.Code != http.StatusNotFound {
		t.Fatalf("share token authorized a different artifact: %d", wrongArtifact.Code)
	}
	revokeRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/artifact-share-links/"+created.Link.ID, nil)
	revokeRequest.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
	revokeResponse := httptest.NewRecorder()
	handler.ServeHTTP(revokeResponse, revokeRequest)
	var revoked storage.ArtifactShareLink
	if revokeResponse.Code != http.StatusOK || json.Unmarshal(revokeResponse.Body.Bytes(), &revoked) != nil || revoked.RevokedAt == nil {
		t.Fatalf("share-link revocation was not read back: %d %s", revokeResponse.Code, revokeResponse.Body.String())
	}
	revokedDownload := httptest.NewRecorder()
	handler.ServeHTTP(revokedDownload, httptest.NewRequest(http.MethodGet, created.Path, nil))
	if revokedDownload.Code != http.StatusNotFound {
		t.Fatalf("revoked share link still grants artifact access: %d %s", revokedDownload.Code, revokedDownload.Body.String())
	}
	listRequest := httptest.NewRequest(http.MethodGet, "/api/v1/artifact-share-links?artifactId="+artifact.ID, nil)
	listRequest.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, listRequest)
	var links []storage.ArtifactShareLink
	if listResponse.Code != http.StatusOK || json.Unmarshal(listResponse.Body.Bytes(), &links) != nil || len(links) != 1 || links[0].RevokedAt == nil {
		t.Fatalf("share-link management list did not read back revocation: %d %s", listResponse.Code, listResponse.Body.String())
	}
}

func TestNodeLeaseCanTransferTaskArtifactsEndToEnd(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	node, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_artifact_transfer", UserID: owner, Name: "Transfer node", Platform: "linux"}, hashToken("node-artifact-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, hashToken("node-artifact-secret"), "linux", nil, now); err != nil {
		t.Fatal(err)
	}
	inputContent := []byte("immutable project snapshot")
	inputHash := sha256.Sum256(inputContent)
	artifactService, err := artifactstore.New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	inputArtifact, err := artifactService.StoreFromReader(ctx, owner, "project.tar", "application/x-tar", "input-artifact", int64(len(inputContent)), hex.EncodeToString(inputHash[:]), bytes.NewReader(inputContent), now)
	if err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"kind":"agent_turn","content":"continue"}`)
	if _, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: "task_artifact_transfer", UserID: owner, NodeID: node.ID, IdempotencyKey: "artifact-transfer", Payload: payload}, false, now); err != nil {
		t.Fatal(err)
	}
	leaseToken := "node-task-lease-secret"
	if _, err := store.ClaimExecutionTask(ctx, node.ID, hashToken(leaseToken), now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if err := store.AttachExecutionTaskArtifact(ctx, owner, "task_artifact_transfer", inputArtifact.ID, "project_snapshot", now); err != nil {
		t.Fatal(err)
	}
	if err := store.AttachExecutionTaskArtifactForLease(ctx, node.ID, "task_artifact_transfer", "wrong-lease-hash", inputArtifact.ID, "forged_output", now); err == nil {
		t.Fatal("artifact association accepted an invalid node task lease")
	}
	unchanged, err := store.ExecutionTaskArtifacts(ctx, owner, "task_artifact_transfer")
	if err != nil || len(unchanged) != 1 || unchanged[0].Role != "project_snapshot" {
		t.Fatalf("failed lease authorization left a published artifact association: items=%+v err=%v", unchanged, err)
	}
	server := &Server{workspaceID: owner, store: store, artifacts: artifactService}
	server.SetRemoteAuthentication("01234567890123456789012345678901")
	handler := server.Handler()
	request := func(method, path string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer node-artifact-secret")
		req.Header.Set("X-O-Task-Lease", leaseToken)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	manifestResponse := request(http.MethodGet, "/api/v1/nodes/tasks/task_artifact_transfer/artifacts", nil)
	var manifest []storage.ExecutionTaskArtifact
	if manifestResponse.Code != http.StatusOK || json.Unmarshal(manifestResponse.Body.Bytes(), &manifest) != nil || len(manifest) != 1 || manifest[0].ID != inputArtifact.ID || manifest[0].SHA256 != hex.EncodeToString(inputHash[:]) {
		t.Fatalf("node input manifest did not read back: status=%d body=%s", manifestResponse.Code, manifestResponse.Body.String())
	}
	inputResponse := request(http.MethodGet, "/api/v1/nodes/tasks/task_artifact_transfer/artifacts/"+inputArtifact.ID, nil)
	if inputResponse.Code != http.StatusOK || !bytes.Equal(inputResponse.Body.Bytes(), inputContent) || inputResponse.Header().Get("X-Artifact-SHA256") != hex.EncodeToString(inputHash[:]) {
		t.Fatalf("node could not download verified input: status=%d body=%q", inputResponse.Code, inputResponse.Body.Bytes())
	}
	ticketResponse := request(http.MethodGet, "/api/v1/nodes/tasks/task_artifact_transfer/artifact-downloads/"+inputArtifact.ID+"/ticket", nil)
	var ticket nodeTaskArtifactDownloadTicket
	if ticketResponse.Code != http.StatusOK || json.Unmarshal(ticketResponse.Body.Bytes(), &ticket) != nil || ticket.TaskID != "task_artifact_transfer" || ticket.ArtifactID != inputArtifact.ID || ticket.SHA256 != hex.EncodeToString(inputHash[:]) || ticket.ByteSize != int64(len(inputContent)) || ticket.Direct || ticket.URL != "" || !ticket.ExpiresAt.IsZero() || ticketResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("local backend did not issue an authenticated task-scoped proxy ticket: status=%d body=%s ticket=%+v", ticketResponse.Code, ticketResponse.Body.String(), ticket)
	}
	foreignTicket := request(http.MethodGet, "/api/v1/nodes/tasks/another-task/artifact-downloads/"+inputArtifact.ID+"/ticket", nil)
	if foreignTicket.Code == http.StatusOK {
		t.Fatal("node lease for one task obtained a direct download capability for a foreign task artifact")
	}

	outputContent := []byte("verified node output")
	outputHash := sha256.Sum256(outputContent)
	beginBody, _ := json.Marshal(map[string]any{"fileName": "result.txt", "mediaType": "text/plain", "expectedSize": len(outputContent), "expectedSha256": hex.EncodeToString(outputHash[:])})
	beginRequest := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/tasks/task_artifact_transfer/artifacts/uploads", bytes.NewReader(beginBody))
	beginRequest.Header.Set("Authorization", "Bearer node-artifact-secret")
	beginRequest.Header.Set("X-O-Task-Lease", leaseToken)
	beginRequest.Header.Set("Idempotency-Key", "output-01")
	beginResponse := httptest.NewRecorder()
	handler.ServeHTTP(beginResponse, beginRequest)
	var begun struct {
		Upload storage.ArtifactUpload `json:"upload"`
	}
	if beginResponse.Code != http.StatusCreated || json.Unmarshal(beginResponse.Body.Bytes(), &begun) != nil || begun.Upload.ID == "" {
		t.Fatalf("node output upload could not begin: status=%d body=%s", beginResponse.Code, beginResponse.Body.String())
	}
	chunkRequest := httptest.NewRequest(http.MethodPut, "/api/v1/nodes/tasks/task_artifact_transfer/artifacts/uploads/"+begun.Upload.ID+"/chunks/0", bytes.NewReader(outputContent))
	chunkRequest.Header.Set("Authorization", "Bearer node-artifact-secret")
	chunkRequest.Header.Set("X-O-Task-Lease", leaseToken)
	chunkRequest.Header.Set("X-Chunk-SHA256", hex.EncodeToString(outputHash[:]))
	chunkResponse := httptest.NewRecorder()
	handler.ServeHTTP(chunkResponse, chunkRequest)
	if chunkResponse.Code != http.StatusOK {
		t.Fatalf("node output chunk failed: status=%d body=%s", chunkResponse.Code, chunkResponse.Body.String())
	}
	statusResponse := request(http.MethodGet, "/api/v1/nodes/tasks/task_artifact_transfer/artifacts/uploads/"+begun.Upload.ID+"?chunkOffset=0&chunkLimit=10", nil)
	var uploadStatus struct {
		Upload storage.ArtifactUpload  `json:"upload"`
		Chunks []storage.ArtifactChunk `json:"chunks"`
		Count  int64                   `json:"receivedChunkCount"`
	}
	if statusResponse.Code != http.StatusOK || json.Unmarshal(statusResponse.Body.Bytes(), &uploadStatus) != nil || uploadStatus.Upload.ID != begun.Upload.ID || uploadStatus.Count != 1 || len(uploadStatus.Chunks) != 1 || uploadStatus.Chunks[0].SHA256 != hex.EncodeToString(outputHash[:]) {
		t.Fatalf("node could not resume from authoritative chunk state: status=%d body=%s", statusResponse.Code, statusResponse.Body.String())
	}
	finalizeBody, _ := json.Marshal(map[string]string{"role": "result_bundle"})
	finalizeResponse := request(http.MethodPost, "/api/v1/nodes/tasks/task_artifact_transfer/artifacts/uploads/"+begun.Upload.ID+"/finalize", finalizeBody)
	var outputArtifact storage.ExecutionTaskArtifact
	if finalizeResponse.Code != http.StatusOK || json.Unmarshal(finalizeResponse.Body.Bytes(), &outputArtifact) != nil || outputArtifact.Role != "result_bundle" || outputArtifact.SHA256 != hex.EncodeToString(outputHash[:]) {
		t.Fatalf("node output was not durably attached: status=%d body=%s", finalizeResponse.Code, finalizeResponse.Body.String())
	}
	readBack, err := store.ExecutionTaskArtifacts(ctx, owner, "task_artifact_transfer")
	var persistedOutput bool
	for _, item := range readBack {
		if item.ID == outputArtifact.ID && item.Role == "result_bundle" && item.SHA256 == hex.EncodeToString(outputHash[:]) {
			persistedOutput = true
		}
	}
	if err != nil || len(readBack) != 2 || !persistedOutput {
		t.Fatalf("task artifact index did not persist node output: items=%+v err=%v", readBack, err)
	}
	foreignTaskResponse := request(http.MethodGet, "/api/v1/nodes/tasks/another-task/artifacts/"+inputArtifact.ID, nil)
	if foreignTaskResponse.Code == http.StatusOK {
		t.Fatal("node lease for one task accessed an artifact attached to a different task")
	}
	wrongLeaseRequest := httptest.NewRequest(http.MethodGet, "/api/v1/nodes/tasks/task_artifact_transfer/artifacts", nil)
	wrongLeaseRequest.Header.Set("Authorization", "Bearer node-artifact-secret")
	wrongLeaseRequest.Header.Set("X-O-Task-Lease", "incorrect-lease")
	wrongLeaseResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongLeaseResponse, wrongLeaseRequest)
	if wrongLeaseResponse.Code == http.StatusOK {
		t.Fatal("invalid task lease accessed task artifacts")
	}
}

func TestNodeCanUploadOnlyBoundRecoveryEvidenceAfterLeaseExpiry(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	const nodeCredential = "node-recovery-evidence-secret"
	const leaseToken = "expired-node-task-lease-secret"
	node, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_recovery_evidence", UserID: owner, Name: "Recovery laptop", Platform: "linux"}, hashToken(nodeCredential))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, hashToken(nodeCredential), "linux", nil, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: "task_recovery_evidence", UserID: owner, NodeID: node.ID, IdempotencyKey: "recovery-evidence", Payload: json.RawMessage(`{"kind":"agent_turn"}`)}, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExecutionTask(ctx, node.ID, hashToken(leaseToken), now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if count, err := store.ReconcileExpiredExecutionTaskLeasesForUser(ctx, owner, now.Add(2*time.Minute)); err != nil || count != 1 {
		t.Fatalf("expire node task lease: count=%d err=%v", count, err)
	}
	artifactService, err := artifactstore.New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{workspaceID: owner, store: store, artifacts: artifactService}
	server.SetRemoteAuthentication("01234567890123456789012345678901")
	handler := server.Handler()
	body := []byte("verified local transcript evidence")
	digest := sha256.Sum256(body)
	digestHex := hex.EncodeToString(digest[:])
	request := func(method, path string, payload []byte, idempotencyKey string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+nodeCredential)
		req.Header.Set("X-O-Task-Lease", leaseToken)
		if idempotencyKey != "" {
			req.Header.Set("Idempotency-Key", idempotencyKey)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	beginBody, _ := json.Marshal(map[string]any{"fileName": "transcript.json", "mediaType": "application/json", "expectedSize": len(body), "expectedSha256": digestHex, "role": "recovery_transcript"})
	base := "/api/v1/nodes/tasks/task_recovery_evidence/recovery-evidence/artifacts/uploads"
	forgedBody, _ := json.Marshal(map[string]any{"fileName": "result.json", "mediaType": "application/json", "expectedSize": len(body), "expectedSha256": digestHex, "role": "result_bundle"})
	if response := request(http.MethodPost, base, forgedBody, "forged-recovery-role"); response.Code >= 200 && response.Code < 300 {
		t.Fatal("recovery lease started an artifact upload outside the evidence role allowlist")
	}
	beginResponse := request(http.MethodPost, base, beginBody, "recovery-transcript")
	var begin struct {
		Upload storage.ArtifactUpload `json:"upload"`
	}
	if beginResponse.Code != http.StatusCreated || json.Unmarshal(beginResponse.Body.Bytes(), &begin) != nil || begin.Upload.ID == "" {
		t.Fatalf("recovery evidence upload begin failed: status=%d body=%s", beginResponse.Code, beginResponse.Body.String())
	}
	chunkPath := base + "/" + begin.Upload.ID + "/chunks/0"
	chunkRequest := httptest.NewRequest(http.MethodPut, chunkPath, bytes.NewReader(body))
	chunkRequest.Header.Set("Authorization", "Bearer "+nodeCredential)
	chunkRequest.Header.Set("X-O-Task-Lease", leaseToken)
	chunkRequest.Header.Set("X-Chunk-SHA256", digestHex)
	chunkResponse := httptest.NewRecorder()
	handler.ServeHTTP(chunkResponse, chunkRequest)
	if chunkResponse.Code != http.StatusOK {
		t.Fatalf("recovery evidence chunk upload failed: status=%d body=%s", chunkResponse.Code, chunkResponse.Body.String())
	}
	finalizeBody, _ := json.Marshal(map[string]string{"role": "recovery_transcript"})
	finalizeResponse := request(http.MethodPost, base+"/"+begin.Upload.ID+"/finalize", finalizeBody, "")
	var attached storage.ExecutionTaskArtifact
	if finalizeResponse.Code != http.StatusOK || json.Unmarshal(finalizeResponse.Body.Bytes(), &attached) != nil || attached.Role != "recovery_transcript" || attached.SHA256 != digestHex {
		t.Fatalf("recovery evidence did not attach: status=%d body=%s", finalizeResponse.Code, finalizeResponse.Body.String())
	}
	task, err := store.ExecutionTask(ctx, owner, "task_recovery_evidence")
	if err != nil || task.Status != "needs_reconciliation" || task.LeaseUntil != nil || task.Sequence != 4 {
		t.Fatalf("evidence upload changed task execution state: task=%+v err=%v", task, err)
	}
	items, err := store.ExecutionTaskArtifacts(ctx, owner, task.ID)
	if err != nil || len(items) != 1 || items[0].ID != attached.ID || items[0].Role != "recovery_transcript" {
		t.Fatalf("recovery evidence association not read back: items=%+v err=%v", items, err)
	}
	recoveryResult, _ := json.Marshal(map[string]any{"sourceConversationId": "source", "conversationId": "local", "agentTurnId": "turn", "resultMessageId": "message", "agentStatus": "completed", "agentStopReason": "assistant_response", "transcriptArtifact": attached})
	resultResponse := request(http.MethodPost, "/api/v1/nodes/tasks/task_recovery_evidence/recovery-evidence/result", recoveryResult, "")
	var recoveryReadBack storage.ExecutionTask
	if resultResponse.Code != http.StatusOK || json.Unmarshal(resultResponse.Body.Bytes(), &recoveryReadBack) != nil || recoveryReadBack.Status != "needs_reconciliation" || !bytes.Equal(recoveryReadBack.RecoveryResult, recoveryResult) || recoveryReadBack.Sequence != 5 {
		t.Fatalf("recovery manifest did not read back durably: status=%d body=%s", resultResponse.Code, resultResponse.Body.String())
	}
	resultRetry := request(http.MethodPost, "/api/v1/nodes/tasks/task_recovery_evidence/recovery-evidence/result", recoveryResult, "")
	if resultRetry.Code != http.StatusOK {
		t.Fatalf("identical recovery manifest retry failed: %d %s", resultRetry.Code, resultRetry.Body.String())
	}
	events, err := store.ExecutionTaskEvents(ctx, owner, task.ID)
	if err != nil || len(events) != 5 || events[3].Kind != "recovery_evidence_attached" || events[4].Kind != "recovery_result_verified" {
		t.Fatalf("recovery evidence event not read back: events=%+v err=%v", events, err)
	}
	for _, path := range []string{
		"/api/v1/nodes/tasks/task_recovery_evidence/report",
		"/api/v1/nodes/tasks/task_recovery_evidence/pulse",
	} {
		response := request(http.MethodPost, path, []byte(`{"status":"reported_succeeded","result":{}}`), "")
		if response.Code >= 200 && response.Code < 300 {
			t.Fatalf("expired lease regained execution permission on %s", path)
		}
	}
	if count, err := store.ReconcileExpiredExecutionTaskLeasesForUser(ctx, owner, now.Add(3*time.Minute)); err != nil || count != 0 {
		t.Fatalf("repeated lease reconciliation changed task: count=%d err=%v", count, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.ExecutionTask(ctx, owner, "task_recovery_evidence")
	if err != nil || task.Status != "needs_reconciliation" || !bytes.Equal(task.RecoveryResult, recoveryResult) || task.Sequence != 5 {
		t.Fatalf("recovery manifest did not survive restart: task=%+v err=%v", task, err)
	}
}
