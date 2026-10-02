package artifactstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"axiom.local/agent/internal/storage"
)

type fakeS3 struct {
	mu         sync.Mutex
	objects    map[string][]byte
	uploads    map[string]*fakeS3Upload
	failGet    bool
	corruptGet bool
	nextUpload int
	requests   int
}

type fakeS3Upload struct {
	key   string
	parts map[int][]byte
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}, uploads: map[string]*fakeS3Upload{}}
}

func (fake *fakeS3) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if err := validateFakeS3SigV4(request); err != nil {
		http.Error(writer, err.Error(), http.StatusUnauthorized)
		return
	}
	fake.mu.Lock()
	fake.requests++
	key := strings.TrimPrefix(request.URL.Path, "/bucket/")
	query := request.URL.Query()
	if request.Method == http.MethodGet {
		object, exists := fake.objects[key]
		fail, corrupt := fake.failGet, fake.corruptGet
		fake.failGet, fake.corruptGet = false, false
		fake.mu.Unlock()
		if fail || !exists {
			http.Error(writer, "missing object", http.StatusNotFound)
			return
		}
		if corrupt && len(object) > 0 {
			object = append([]byte(nil), object...)
			object[0] ^= 0xff
		}
		writer.Header().Set("Content-Length", fmt.Sprint(len(object)))
		if disposition := query.Get("response-content-disposition"); disposition != "" {
			writer.Header().Set("Content-Disposition", disposition)
		}
		if cacheControl := query.Get("response-cache-control"); cacheControl != "" {
			writer.Header().Set("Cache-Control", cacheControl)
		}
		if contentType := query.Get("response-content-type"); contentType != "" {
			writer.Header().Set("Content-Type", contentType)
		}
		_, _ = writer.Write(object)
		return
	}
	if request.Method == http.MethodHead {
		object, exists := fake.objects[key]
		etag := fakeS3ETag(object)
		fake.mu.Unlock()
		if !exists {
			http.Error(writer, "missing object", http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Length", fmt.Sprint(len(object)))
		writer.Header().Set("ETag", etag)
		writer.WriteHeader(http.StatusOK)
		return
	}
	if request.Method == http.MethodDelete {
		delete(fake.objects, key)
		fake.mu.Unlock()
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if request.Method == http.MethodPut && request.Header.Get("x-amz-copy-source") != "" {
		source := strings.TrimPrefix(request.Header.Get("x-amz-copy-source"), "/bucket/")
		object, exists := fake.objects[source]
		if !exists || fakeS3ETag(object) != request.Header.Get("x-amz-copy-source-if-match") {
			fake.mu.Unlock()
			http.Error(writer, "copy source precondition failed", http.StatusPreconditionFailed)
			return
		}
		fake.objects[key] = append([]byte(nil), object...)
		fake.mu.Unlock()
		_, _ = writer.Write([]byte("<CopyObjectResult><ETag>" + strings.Trim(fakeS3ETag(object), `"`) + "</ETag></CopyObjectResult>"))
		return
	}
	if request.Method == http.MethodPut && query.Get("uploadId") != "" {
		upload := fake.uploads[query.Get("uploadId")]
		partNumber := 0
		_, parseErr := fmt.Sscanf(query.Get("partNumber"), "%d", &partNumber)
		if upload == nil || parseErr != nil || partNumber < 1 {
			fake.mu.Unlock()
			http.Error(writer, "bad multipart part", http.StatusBadRequest)
			return
		}
		fake.mu.Unlock()
		content, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, "read part", http.StatusBadRequest)
			return
		}
		digest := md5.Sum(content)
		fake.mu.Lock()
		upload.parts[partNumber] = content
		fake.mu.Unlock()
		writer.Header().Set("ETag", `"`+hex.EncodeToString(digest[:])+`"`)
		writer.WriteHeader(http.StatusOK)
		return
	}
	if request.Method == http.MethodPut {
		fake.mu.Unlock()
		content, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, "read object", http.StatusBadRequest)
			return
		}
		fake.mu.Lock()
		fake.objects[key] = content
		fake.mu.Unlock()
		writer.WriteHeader(http.StatusOK)
		return
	}
	if request.Method == http.MethodPost && query.Has("uploads") {
		fake.nextUpload++
		uploadID := fmt.Sprintf("upload-%d", fake.nextUpload)
		fake.uploads[uploadID] = &fakeS3Upload{key: key, parts: map[int][]byte{}}
		fake.mu.Unlock()
		writer.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(writer, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", uploadID)
		return
	}
	if request.Method == http.MethodPost && query.Get("uploadId") != "" {
		upload := fake.uploads[query.Get("uploadId")]
		fake.mu.Unlock()
		if upload == nil {
			http.Error(writer, "unknown upload", http.StatusBadRequest)
			return
		}
		var completion struct {
			XMLName xml.Name          `xml:"CompleteMultipartUpload"`
			Parts   []s3CompletedPart `xml:"Part"`
		}
		if err := xml.NewDecoder(request.Body).Decode(&completion); err != nil || completion.XMLName.Space != "http://s3.amazonaws.com/doc/2006-03-01/" || len(completion.Parts) == 0 {
			http.Error(writer, "bad completion", http.StatusBadRequest)
			return
		}
		var content bytes.Buffer
		for _, part := range completion.Parts {
			content.Write(upload.parts[part.PartNumber])
		}
		fake.mu.Lock()
		fake.objects[upload.key] = content.Bytes()
		delete(fake.uploads, query.Get("uploadId"))
		fake.mu.Unlock()
		_, _ = writer.Write([]byte("<CompleteMultipartUploadResult><ETag>done</ETag></CompleteMultipartUploadResult>"))
		return
	}
	if request.Method == http.MethodDelete && query.Get("uploadId") != "" {
		delete(fake.uploads, query.Get("uploadId"))
		fake.mu.Unlock()
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	fake.mu.Unlock()
	http.Error(writer, "unsupported fake S3 request", http.StatusNotImplemented)
}

func fakeS3ETag(object []byte) string {
	digest := md5.Sum(object)
	return `"` + hex.EncodeToString(digest[:]) + `"`
}

func TestPresignedGetDispositionIsCoveredBySigV4(t *testing.T) {
	fake := newFakeS3()
	server := httptest.NewServer(fake)
	defer server.Close()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	service, err := NewWithS3(t.TempDir(), store, S3Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", AccessKeyID: "test-access", SecretAccessKey: "test-secret", Prefix: "o-agent/test", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	capability, expiry, err := service.remote.presignedGetURL("sha256/ab/example", time.Minute, "attachment; filename*=UTF-8''report%20final.txt")
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, capability, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateFakeS3PresignedURL(request); err != nil {
		t.Fatalf("response disposition was not covered by the presigned signature: %v", err)
	}
	if request.URL.Query().Get("response-content-disposition") != "attachment; filename*=UTF-8''report%20final.txt" || time.Until(expiry) > time.Minute || time.Until(expiry) <= 0 {
		t.Fatalf("unexpected signed response metadata or expiry: query=%s expiry=%s", request.URL.RawQuery, expiry)
	}
	if request.URL.Query().Get("response-cache-control") != "private, no-store" || request.URL.Query().Get("response-content-type") != "application/octet-stream" {
		t.Fatalf("direct downloads must be private, non-cacheable attachments: query=%s", request.URL.RawQuery)
	}
}

func validateFakeS3SigV4(request *http.Request) error {
	authorization := request.Header.Get("Authorization")
	if authorization == "" {
		if request.Method == http.MethodPut {
			return validateFakeS3PresignedPUT(request)
		}
		return validateFakeS3PresignedURL(request)
	}
	algorithm, parameters, found := strings.Cut(authorization, " ")
	if !found || algorithm != "AWS4-HMAC-SHA256" {
		return fmt.Errorf("missing SigV4 Authorization header")
	}
	var accessScope, signedHeaders, signature string
	for _, field := range strings.Split(parameters, ",") {
		name, value, found := strings.Cut(strings.TrimSpace(field), "=")
		if !found {
			return fmt.Errorf("malformed SigV4 Authorization field")
		}
		switch name {
		case "Credential":
			accessScope = value
		case "SignedHeaders":
			signedHeaders = value
		case "Signature":
			signature = value
		}
	}
	credentialParts := strings.Split(accessScope, "/")
	if len(credentialParts) != 5 || credentialParts[0] != "test-access" || credentialParts[4] != "aws4_request" || signedHeaders == "" || signature == "" {
		return fmt.Errorf("malformed SigV4 credential scope")
	}
	payloadHash := request.Header.Get("x-amz-content-sha256")
	if payloadHash == "" {
		return fmt.Errorf("missing SigV4 payload hash")
	}
	if request.Body != nil {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return fmt.Errorf("read signed request body: %w", err)
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		actualPayloadHash := sha256.Sum256(body)
		if hex.EncodeToString(actualPayloadHash[:]) != payloadHash {
			return fmt.Errorf("SigV4 payload hash does not match request body")
		}
	}
	var headers []string
	for _, name := range strings.Split(signedHeaders, ";") {
		value := request.Header.Get(name)
		if name == "host" {
			value = request.Host
		}
		if value == "" {
			return fmt.Errorf("missing signed request header %q", name)
		}
		headers = append(headers, name+":"+strings.Join(strings.Fields(value), " "))
	}
	canonicalRequest := strings.Join([]string{request.Method, request.URL.EscapedPath(), request.URL.RawQuery, strings.Join(headers, "\n") + "\n", signedHeaders, payloadHash}, "\n")
	requestHash := sha256.Sum256([]byte(canonicalRequest))
	dateTime := request.Header.Get("x-amz-date")
	if dateTime == "" {
		return fmt.Errorf("missing SigV4 timestamp")
	}
	scope := strings.Join(credentialParts[1:], "/")
	stringToSign := "AWS4-HMAC-SHA256\n" + dateTime + "\n" + scope + "\n" + hex.EncodeToString(requestHash[:])
	kDate := fakeHMAC([]byte("AWS4test-secret"), credentialParts[1])
	kRegion := fakeHMAC(kDate, credentialParts[2])
	kService := fakeHMAC(kRegion, credentialParts[3])
	kSigning := fakeHMAC(kService, "aws4_request")
	expected := hex.EncodeToString(fakeHMAC(kSigning, stringToSign))
	if !hmac.Equal([]byte(signature), []byte(expected)) {
		return fmt.Errorf("SigV4 signature did not match independently reconstructed canonical request")
	}
	return nil
}

func validateFakeS3PresignedPUT(request *http.Request) error {
	query := request.URL.Query()
	if query.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" || query.Get("X-Amz-SignedHeaders") != "host;x-amz-checksum-sha256" {
		return fmt.Errorf("missing checksum-bound SigV4 PUT parameters")
	}
	credentialParts := strings.Split(query.Get("X-Amz-Credential"), "/")
	expires, err := strconv.ParseInt(query.Get("X-Amz-Expires"), 10, 64)
	if err != nil || expires < 1 || expires > 15*60 || len(credentialParts) != 5 || credentialParts[0] != "test-access" || credentialParts[4] != "aws4_request" {
		return fmt.Errorf("invalid SigV4 PUT credential or expiry")
	}
	checksum := request.Header.Get("x-amz-checksum-sha256")
	if checksum == "" {
		return fmt.Errorf("missing required SHA-256 checksum header")
	}
	dateTime := query.Get("X-Amz-Date")
	queryValues := map[string]string{}
	providedSignature := query.Get("X-Amz-Signature")
	for key, values := range query {
		if key == "X-Amz-Signature" {
			continue
		}
		if len(values) != 1 {
			return fmt.Errorf("duplicate SigV4 query parameter")
		}
		queryValues[key] = values[0]
	}
	canonicalHeaders := "host:" + request.Host + "\nx-amz-checksum-sha256:" + checksum + "\n"
	canonicalRequest := strings.Join([]string{http.MethodPut, request.URL.EscapedPath(), awsCanonicalQuery(queryValues), canonicalHeaders, "host;x-amz-checksum-sha256", "UNSIGNED-PAYLOAD"}, "\n")
	requestHash := sha256.Sum256([]byte(canonicalRequest))
	scope := strings.Join(credentialParts[1:], "/")
	stringToSign := "AWS4-HMAC-SHA256\n" + dateTime + "\n" + scope + "\n" + hex.EncodeToString(requestHash[:])
	kDate := fakeHMAC([]byte("AWS4test-secret"), credentialParts[1])
	kRegion := fakeHMAC(kDate, credentialParts[2])
	kService := fakeHMAC(kRegion, credentialParts[3])
	kSigning := fakeHMAC(kService, "aws4_request")
	if !hmac.Equal([]byte(providedSignature), []byte(hex.EncodeToString(fakeHMAC(kSigning, stringToSign)))) {
		return fmt.Errorf("checksum-bound SigV4 PUT signature mismatch")
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return err
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	digest := sha256.Sum256(body)
	if base64.StdEncoding.EncodeToString(digest[:]) != checksum {
		return fmt.Errorf("PUT request body did not match signed checksum")
	}
	return nil
}

func validateFakeS3PresignedURL(request *http.Request) error {
	query := request.URL.Query()
	if query.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" || query.Get("X-Amz-SignedHeaders") != "host" {
		return fmt.Errorf("missing SigV4 presigned URL parameters")
	}
	credentialParts := strings.Split(query.Get("X-Amz-Credential"), "/")
	expires, err := strconv.ParseInt(query.Get("X-Amz-Expires"), 10, 64)
	if err != nil || expires < 1 || expires > 7*24*60*60 || len(credentialParts) != 5 || credentialParts[0] != "test-access" || credentialParts[4] != "aws4_request" {
		return fmt.Errorf("invalid SigV4 presigned credential or expiry")
	}
	dateTime := query.Get("X-Amz-Date")
	signedAt, err := time.Parse("20060102T150405Z", dateTime)
	if err != nil || time.Now().UTC().Before(signedAt.Add(-time.Minute)) || time.Now().UTC().After(signedAt.Add(time.Duration(expires)*time.Second)) {
		return fmt.Errorf("SigV4 presigned URL is outside its validity window")
	}
	providedSignature := query.Get("X-Amz-Signature")
	if providedSignature == "" {
		return fmt.Errorf("missing SigV4 presigned signature")
	}
	queryValues := map[string]string{}
	for key, values := range query {
		if key == "X-Amz-Signature" {
			continue
		}
		if len(values) != 1 {
			return fmt.Errorf("duplicate SigV4 query parameter")
		}
		queryValues[key] = values[0]
	}
	scope := strings.Join(credentialParts[1:], "/")
	canonicalQuery := awsCanonicalQuery(queryValues)
	canonicalRequest := strings.Join([]string{request.Method, request.URL.EscapedPath(), canonicalQuery, "host:" + request.Host + "\n", "host", "UNSIGNED-PAYLOAD"}, "\n")
	requestHash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "AWS4-HMAC-SHA256\n" + dateTime + "\n" + scope + "\n" + hex.EncodeToString(requestHash[:])
	kDate := fakeHMAC([]byte("AWS4test-secret"), credentialParts[1])
	kRegion := fakeHMAC(kDate, credentialParts[2])
	kService := fakeHMAC(kRegion, credentialParts[3])
	kSigning := fakeHMAC(kService, "aws4_request")
	expected := hex.EncodeToString(fakeHMAC(kSigning, stringToSign))
	if !hmac.Equal([]byte(providedSignature), []byte(expected)) {
		return fmt.Errorf("SigV4 presigned signature did not match independently reconstructed canonical request")
	}
	if token := query.Get("X-Amz-Security-Token"); token != "" && token != "test-session-token" {
		return fmt.Errorf("presigned URL has an unexpected session token")
	}
	return nil
}

func fakeHMAC(key []byte, value string) []byte {
	hasher := hmac.New(sha256.New, key)
	_, _ = io.WriteString(hasher, value)
	return hasher.Sum(nil)
}

func TestS3ArtifactPublishReadbackRestartAndFailureRollback(t *testing.T) {
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
	fake := newFakeS3()
	server := httptest.NewServer(fake)
	defer server.Close()
	config := S3Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", AccessKeyID: "test-access", SecretAccessKey: "test-secret", Prefix: "o-agent/test", PathStyle: true}
	service, err := NewWithS3(dataDir, store, config)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("remote durable artifact")
	digest := sha256.Sum256(content)
	artifact, err := service.StoreFromReader(ctx, owner, "result.txt", "text/plain", "s3-success-key", int64(len(content)), hex.EncodeToString(digest[:]), bytes.NewReader(content), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	capacity, err := service.Capacity(ctx)
	if err != nil || capacity.Backend != "s3" || !capacity.RemoteObjectsAuthoritative || capacity.ObjectBytes != 0 {
		t.Fatalf("S3 backend capacity state is inaccurate: %+v err=%v", capacity, err)
	}
	artifact, file, err := service.Open(ctx, owner, artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	temporaryPath := file.Name()
	readBack, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(readBack, content) || artifact.StorageKey != "sha256/"+hex.EncodeToString(digest[:])[:2]+"/"+hex.EncodeToString(digest[:]) {
		t.Fatalf("remote artifact did not read back by content address: %q err=%v close=%v artifact=%+v", readBack, readErr, closeErr, artifact)
	}
	if _, err := os.Stat(temporaryPath); !os.IsNotExist(err) {
		t.Fatalf("remote download staging file remained after the reader closed: %s err=%v", temporaryPath, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restartedService, err := NewWithS3(dataDir, restarted, config)
	if err != nil {
		t.Fatalf("S3 artifact service did not recover from durable remote objects after restart: %v", err)
	}
	if _, file, err := restartedService.Open(ctx, owner, artifact.ID); err != nil {
		t.Fatalf("restart could not read the finalized remote artifact: %v", err)
	} else if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	failurePayload := []byte("verified only after retry")
	failureDigest := sha256.Sum256(failurePayload)
	upload, _, err := restartedService.Begin(ctx, owner, BeginRequest{FileName: "retry.txt", ExpectedSize: int64(len(failurePayload)), ExpectedSHA256: hex.EncodeToString(failureDigest[:]), IdempotencyKey: "s3-readback-failure"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restartedService.Upload(ctx, owner, upload.ID, 0, "", bytes.NewReader(failurePayload), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.corruptGet = true
	fake.mu.Unlock()
	if _, err := restartedService.Finalize(ctx, owner, upload.ID, time.Now().UTC()); err == nil {
		t.Fatal("corrupt remote readback was finalized as a usable artifact")
	}
	state, err := restarted.ArtifactUpload(ctx, owner, upload.ID)
	if err != nil || state.Status != "uploading" {
		t.Fatalf("failed remote readback was falsely committed or lost its retry state: state=%+v err=%v", state, err)
	}
	if _, err := restartedService.Finalize(ctx, owner, upload.ID, time.Now().UTC()); err != nil {
		t.Fatalf("verified retry could not complete the artifact after remote readback recovered: %v", err)
	}
}

func TestS3BackendMigratesLegacyLocalArtifactOnRead(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	localService, err := New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("pre-existing local artifact")
	digest := sha256.Sum256(content)
	artifact, err := localService.StoreFromReader(ctx, owner, "legacy.txt", "text/plain", "legacy-key", int64(len(content)), hex.EncodeToString(digest[:]), bytes.NewReader(content), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fake := newFakeS3()
	server := httptest.NewServer(fake)
	defer server.Close()
	remoteService, err := NewWithS3(dataDir, store, S3Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", AccessKeyID: "test-access", SecretAccessKey: "test-secret", Prefix: "o-agent/test", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	readArtifact, file, err := remoteService.Open(ctx, owner, artifact.ID)
	if err != nil {
		t.Fatalf("remote backend failed to migrate a valid pre-existing artifact: %v", err)
	}
	readBack, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(readBack, content) || readArtifact.ID != artifact.ID {
		t.Fatalf("legacy artifact did not remain readable after migration: data=%q read=%v close=%v", readBack, readErr, closeErr)
	}
	fake.mu.Lock()
	remoteBytes := append([]byte(nil), fake.objects["o-agent/test/"+artifact.StorageKey]...)
	fake.mu.Unlock()
	if !bytes.Equal(remoteBytes, content) {
		t.Fatalf("legacy local artifact was not committed to remote storage after verified migration: %q", remoteBytes)
	}

	corruptContent := []byte("corrupt before migration")
	corruptDigest := sha256.Sum256(corruptContent)
	localService, err = New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	corruptArtifact, err := localService.StoreFromReader(ctx, owner, "corrupt.txt", "text/plain", "corrupt-legacy-key", int64(len(corruptContent)), hex.EncodeToString(corruptDigest[:]), bytes.NewReader(corruptContent), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localService.objectPath(corruptArtifact.SHA256), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, file, err := remoteService.Open(ctx, owner, corruptArtifact.ID); err == nil {
		_ = file.Close()
		t.Fatal("corrupt legacy bytes were migrated and exposed as a valid artifact")
	}
	fake.mu.Lock()
	_, unexpectedlyMigrated := fake.objects["o-agent/test/"+corruptArtifact.StorageKey]
	fake.mu.Unlock()
	if unexpectedlyMigrated {
		t.Fatal("corrupt legacy object was published remotely before source verification")
	}
}

func TestS3MultipartUploadsPartsAndCommitsBeforeReadback(t *testing.T) {
	fake := newFakeS3()
	server := httptest.NewServer(fake)
	defer server.Close()
	remote, err := newS3ObjectStore(S3Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", AccessKeyID: "test-access", SecretAccessKey: "test-secret", SessionToken: "test-session-token", Prefix: "o-agent/test", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	remote.multipartLimit = s3MinPartSize
	content := bytes.Repeat([]byte("multipart-data"), int((s3MinPartSize/14)+2))
	filePath := filepath.Join(t.TempDir(), "multipart.bin")
	if err := os.WriteFile(filePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	storageKey := "sha256/00/" + hex.EncodeToString(digest[:])
	if err := remote.putObject(context.Background(), storageKey, filePath, int64(len(content))); err != nil {
		t.Fatal(err)
	}
	body, err := remote.openObject(context.Background(), storageKey)
	if err != nil {
		t.Fatal(err)
	}
	readBack, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(readBack, content) {
		t.Fatalf("multipart S3 object did not assemble/read back: size=%d err=%v close=%v", len(readBack), readErr, closeErr)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.uploads) != 0 || len(fake.objects) != 1 {
		t.Fatalf("multipart completion did not commit exactly one object and close its upload: uploads=%d objects=%d", len(fake.uploads), len(fake.objects))
	}
}

func TestS3CanonicalQueryEscapesAndSortsValues(t *testing.T) {
	got := awsCanonicalQuery(map[string]string{"uploadId": "part+/= id", "partNumber": "2", "uploads": ""})
	want := "partNumber=2&uploadId=part%2B%2F%3D%20id&uploads="
	if got != want {
		t.Fatalf("canonical query = %q, want %q", got, want)
	}
}

func TestS3CanonicalPathEscapesWithoutNormalizing(t *testing.T) {
	remote, err := newS3ObjectStore(S3Config{Endpoint: "http://127.0.0.1:9000/gateway", Region: "test-1", Bucket: "bucket", AccessKeyID: "access", SecretAccessKey: "secret", Prefix: "o-agent/a..b", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	target, err := remote.requestURL(remote.objectKey("sha256//segment+ space"), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "/gateway/bucket/o-agent/a..b/sha256//segment%2B%20space"
	if target.EscapedPath() != want {
		t.Fatalf("S3 escaped path = %q, want unnormalized %q", target.EscapedPath(), want)
	}
}

func TestS3PresignedDownloadUsesShortLivedScopedSigV4URL(t *testing.T) {
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
	fake := newFakeS3()
	server := httptest.NewServer(fake)
	defer server.Close()
	config := S3Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", AccessKeyID: "test-access", SecretAccessKey: "test-secret", SessionToken: "test-session-token", Prefix: "o-agent/test", PathStyle: true}
	service, err := NewWithS3(dataDir, store, config)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("download straight from the private object store")
	digest := sha256.Sum256(content)
	artifact, err := service.StoreFromReader(ctx, owner, "artifact.dat", "application/octet-stream", "presigned-test-upload", int64(len(content)), hex.EncodeToString(digest[:]), bytes.NewReader(content), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	readBackArtifact, capability, direct, err := service.CreatePresignedDownloadURL(ctx, owner, artifact.ID, 5*time.Minute)
	if err != nil || !direct || readBackArtifact.ID != artifact.ID || readBackArtifact.SHA256 != artifact.SHA256 || time.Until(capability.ExpiresAt) > 5*time.Minute || time.Until(capability.ExpiresAt) < 4*time.Minute {
		t.Fatalf("could not mint a verified short-lived S3 download capability: artifact=%+v capability=%+v direct=%v err=%v", readBackArtifact, capability, direct, err)
	}
	response, err := http.Get(capability.URL)
	if err != nil {
		t.Fatal(err)
	}
	readBack, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if response.StatusCode != http.StatusOK || readErr != nil || closeErr != nil || !bytes.Equal(readBack, content) {
		t.Fatalf("presigned object did not download and read back: status=%d body=%q read=%v close=%v", response.StatusCode, readBack, readErr, closeErr)
	}
	tampered, err := url.Parse(capability.URL)
	if err != nil {
		t.Fatal(err)
	}
	query := tampered.Query()
	query.Set("X-Amz-Expires", "600")
	tampered.RawQuery = query.Encode()
	response, err = http.Get(tampered.String())
	if err != nil {
		t.Fatal(err)
	}
	closeErr = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || closeErr != nil {
		t.Fatalf("modified presigned download parameters were accepted: status=%d close=%v", response.StatusCode, closeErr)
	}
}

func TestS3DirectUploadVerifiesChecksumPromotesAndPersistsOnRetry(t *testing.T) {
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
	fake := newFakeS3()
	server := httptest.NewServer(fake)
	defer server.Close()
	service, err := NewWithS3(dataDir, store, S3Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", AccessKeyID: "test-access", SecretAccessKey: "test-secret", Prefix: "o-agent/test", PathStyle: true, EnableDirectUpload: true})
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("uploaded directly under a single-object checksum-bound capability")
	digest := sha256.Sum256(content)
	upload, created, err := service.Begin(ctx, owner, BeginRequest{FileName: "direct.bin", ExpectedSize: int64(len(content)), ExpectedSHA256: hex.EncodeToString(digest[:]), IdempotencyKey: "direct-upload-test"}, time.Now().UTC())
	if err != nil || !created {
		t.Fatalf("begin direct upload: created=%v upload=%+v err=%v", created, upload, err)
	}
	readBack, capability, direct, err := service.CreateDirectUploadCapability(ctx, owner, upload.ID, time.Now().UTC())
	if err != nil || !direct || !readBack.DirectUpload || readBack.ID != upload.ID || capability.URL == "" || capability.RequiredHeaders["x-amz-checksum-sha256"] == "" {
		t.Fatalf("direct upload capability did not persist and read back its mode: upload=%+v capability=%+v direct=%v err=%v", readBack, capability, direct, err)
	}
	wrong, _ := http.NewRequest(http.MethodPut, capability.URL, bytes.NewReader([]byte("wrong")))
	wrong.Header.Set("x-amz-checksum-sha256", capability.RequiredHeaders["x-amz-checksum-sha256"])
	wrongResponse, err := http.DefaultClient.Do(wrong)
	if err != nil {
		t.Fatal(err)
	}
	_ = wrongResponse.Body.Close()
	if wrongResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("provider accepted bytes that do not match the signed checksum: status=%d", wrongResponse.StatusCode)
	}
	request, _ := http.NewRequest(http.MethodPut, capability.URL, bytes.NewReader(content))
	request.Header.Set("x-amz-checksum-sha256", capability.RequiredHeaders["x-amz-checksum-sha256"])
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	closeErr := response.Body.Close()
	if response.StatusCode != http.StatusOK || closeErr != nil {
		t.Fatalf("provider rejected checksum-bound direct upload: status=%d close=%v", response.StatusCode, closeErr)
	}
	artifact, err := service.Finalize(ctx, owner, upload.ID, time.Now().UTC())
	if err != nil || artifact.SHA256 != hex.EncodeToString(digest[:]) || artifact.ByteSize != int64(len(content)) {
		t.Fatalf("direct upload was not hash-verified and committed: artifact=%+v err=%v", artifact, err)
	}
	_, stored, err := service.Open(ctx, owner, artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	readBackBytes, readErr := io.ReadAll(stored)
	closeErr = stored.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(readBackBytes, content) {
		t.Fatalf("promoted artifact did not read back: bytes=%q read=%v close=%v", readBackBytes, readErr, closeErr)
	}
	state, err := store.ArtifactUpload(ctx, owner, upload.ID)
	if err != nil || !state.DirectUpload || state.Status != "complete" || state.ArtifactID != artifact.ID {
		t.Fatalf("direct upload completion did not persist: state=%+v err=%v", state, err)
	}
	if _, _, err := fakeObject(fake, "o-agent/test/staging/uploads/"+upload.ID+"/payload"); err == nil {
		t.Fatal("temporary direct-upload object remained after successful completion")
	}
	restartedService, err := NewWithS3(dataDir, store, S3Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", AccessKeyID: "test-access", SecretAccessKey: "test-secret", Prefix: "o-agent/test", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	finalizedAgain, err := restartedService.Finalize(ctx, owner, upload.ID, time.Now().UTC())
	if err != nil || finalizedAgain.ID != artifact.ID || finalizedAgain.SHA256 != artifact.SHA256 {
		t.Fatalf("completed direct upload did not survive service restart and idempotent finalize: artifact=%+v err=%v", finalizedAgain, err)
	}
}

func TestS3DirectUploadRemainsOnResumablePathUnlessProviderIsOptedIn(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	userID, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewWithS3(dataDir, store, S3Config{Endpoint: "http://127.0.0.1:9000", Region: "us-east-1", Bucket: "bucket", AccessKeyID: "access", SecretAccessKey: "secret", Prefix: "o-agent/test", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("keep the compatible retryable proxy upload path")
	digest := sha256.Sum256(content)
	upload, _, err := service.Begin(ctx, userID, BeginRequest{FileName: "result.bin", ExpectedSize: int64(len(content)), ExpectedSHA256: hex.EncodeToString(digest[:]), IdempotencyKey: "s3-direct-opt-in"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	readBack, capability, direct, err := service.CreateDirectUploadCapability(ctx, userID, upload.ID, time.Now().UTC())
	if err != nil || direct || capability.URL != "" || readBack.DirectUpload {
		t.Fatalf("provider not opted in to direct upload did not retain the resumable mode: upload=%+v capability=%+v direct=%v err=%v", readBack, capability, direct, err)
	}
	if _, err := service.Upload(ctx, userID, upload.ID, 0, hex.EncodeToString(digest[:]), bytes.NewReader(content), time.Now().UTC()); err != nil {
		t.Fatalf("fallback proxy chunk upload failed: %v", err)
	}
}

func TestS3DirectUploadCanResumePersistedAttemptAfterOptInIsDisabled(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	userID, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	config := S3Config{Endpoint: "http://127.0.0.1:9000", Region: "us-east-1", Bucket: "bucket", AccessKeyID: "access", SecretAccessKey: "secret", Prefix: "o-agent/test", PathStyle: true, EnableDirectUpload: true}
	firstService, err := NewWithS3(dataDir, store, config)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("durable direct transfer can renew its short-lived capability")
	digest := sha256.Sum256(content)
	upload, _, err := firstService.Begin(ctx, userID, BeginRequest{FileName: "resume.bin", ExpectedSize: int64(len(content)), ExpectedSHA256: hex.EncodeToString(digest[:]), IdempotencyKey: "direct-resume-after-disable"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, direct, err := firstService.CreateDirectUploadCapability(ctx, userID, upload.ID, time.Now().UTC()); err != nil || !direct {
		t.Fatalf("initial direct-upload capability was not issued: direct=%v err=%v", direct, err)
	}
	config.EnableDirectUpload = false
	restartedService, err := NewWithS3(dataDir, store, config)
	if err != nil {
		t.Fatal(err)
	}
	readBack, capability, direct, err := restartedService.CreateDirectUploadCapability(ctx, userID, upload.ID, time.Now().UTC())
	if err != nil || !direct || !readBack.DirectUpload || capability.URL == "" || capability.RequiredHeaders["x-amz-checksum-sha256"] == "" {
		t.Fatalf("disabling new direct uploads stranded an existing durable transfer: upload=%+v capability=%+v direct=%v err=%v", readBack, capability, direct, err)
	}
}

func TestS3DirectUploadThresholdSelectsBatchModeWithoutRejectingLargerArtifacts(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	userID, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeS3()
	server := httptest.NewServer(fake)
	defer server.Close()
	service, err := NewWithS3(dataDir, store, S3Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", AccessKeyID: "test-access", SecretAccessKey: "test-secret", Prefix: "o-agent/test", PathStyle: true, EnableDirectUpload: true})
	if err != nil {
		t.Fatal(err)
	}
	digestBytes := sha256.Sum256([]byte("metadata-only boundary proof"))
	digest := hex.EncodeToString(digestBytes[:])
	for _, testCase := range []struct {
		name       string
		size       int64
		wantDirect bool
	}{
		{name: "maximum single direct batch", size: DirectUploadBatchMaxBytes, wantDirect: true},
		{name: "larger artifact remains resumable", size: DirectUploadBatchMaxBytes + 1, wantDirect: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			upload, _, err := service.Begin(ctx, userID, BeginRequest{FileName: "boundary.bin", ExpectedSize: testCase.size, ExpectedSHA256: digest, IdempotencyKey: "threshold-" + testCase.name}, time.Now().UTC())
			if err != nil || upload.ExpectedSize != testCase.size || upload.Status != "uploading" {
				t.Fatalf("batch threshold rejected or changed artifact size: upload=%+v err=%v", upload, err)
			}
			readBack, capability, direct, err := service.CreateDirectUploadCapability(ctx, userID, upload.ID, time.Now().UTC())
			if err != nil || direct != testCase.wantDirect || readBack.ExpectedSize != testCase.size || readBack.DirectUpload != testCase.wantDirect {
				t.Fatalf("batch threshold selected the wrong retryable mode: upload=%+v capability=%+v direct=%v err=%v", readBack, capability, direct, err)
			}
			if direct != (capability.URL != "") {
				t.Fatalf("capability URL disagrees with selected upload mode: direct=%v URL=%q", direct, capability.URL)
			}
		})
	}
}

func fakeObject(fake *fakeS3, key string) ([]byte, bool, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	object, ok := fake.objects[key]
	if !ok {
		return nil, false, fmt.Errorf("object %q is absent", key)
	}
	return append([]byte(nil), object...), true, nil
}
