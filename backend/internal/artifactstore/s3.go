package artifactstore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
)

const (
	s3MultipartThreshold = int64(100 << 20)
	s3MinPartSize        = int64(5 << 20)
	s3MaxPartSize        = int64(5 << 30)
	s3MaxParts           = int64(10000)
)

// S3Config selects a private S3-compatible object store. Credentials are
// supplied by the process environment and are never returned by this package.
type S3Config struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Prefix          string
	PathStyle       bool
	// EnableDirectUpload opts new uploads into checksum-bound presigned PUT and
	// conditional CopyObject promotion. Keep false until the configured provider
	// is verified; already-persisted direct uploads remain resumable after restart.
	EnableDirectUpload bool
}

type s3ObjectStore struct {
	endpoint        url.URL
	region          string
	bucket          string
	accessKeyID     string
	secretAccessKey string
	sessionToken    string
	prefix          string
	pathStyle       bool
	directUpload    bool
	client          *http.Client
	clock           func() time.Time
	multipartLimit  int64
}

type s3HTTPError struct {
	method string
	status int
	body   string
}

func (failure *s3HTTPError) Error() string {
	return fmt.Sprintf("S3 %s returned HTTP %d: %s", failure.method, failure.status, failure.body)
}

func newS3ObjectStore(config S3Config) (*s3ObjectStore, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || !endpoint.IsAbs() || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return nil, errors.New("S3 endpoint must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if endpoint.Scheme != "https" && !isLoopbackS3Host(endpoint.Hostname()) {
		return nil, errors.New("S3 endpoint must use HTTPS outside loopback testing")
	}
	if strings.TrimSpace(config.Region) == "" || !validS3BucketName(config.Bucket) || strings.TrimSpace(config.AccessKeyID) == "" || strings.TrimSpace(config.SecretAccessKey) == "" {
		return nil, errors.New("S3 region, bucket, access key and secret key are required")
	}
	prefix := config.Prefix
	if prefix == "" || strings.TrimSpace(prefix) != prefix || strings.HasPrefix(prefix, "/") || strings.ContainsAny(prefix, "\\:\x00\r\n") || path.Clean(prefix) != prefix || prefix == "." || prefix == ".." || strings.HasPrefix(prefix, "../") {
		return nil, errors.New("S3 key prefix is invalid")
	}
	// Bound connection setup and response headers, while leaving large object
	// bodies governed by the caller's context instead of a fixed transfer cap.
	client := &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
	}}
	return &s3ObjectStore{
		endpoint: *endpoint, region: config.Region, bucket: config.Bucket,
		accessKeyID: config.AccessKeyID, secretAccessKey: config.SecretAccessKey,
		sessionToken: config.SessionToken, prefix: prefix, pathStyle: config.PathStyle, directUpload: config.EnableDirectUpload,
		client: client, clock: time.Now, multipartLimit: s3MultipartThreshold,
	}, nil
}

func validS3BucketName(bucket string) bool {
	if len(bucket) < 3 || len(bucket) > 63 || net.ParseIP(bucket) != nil || strings.Contains(bucket, "..") {
		return false
	}
	for _, label := range strings.Split(bucket, ".") {
		if label == "" || !isS3AlphaNumeric(label[0]) || !isS3AlphaNumeric(label[len(label)-1]) {
			return false
		}
		for index := 0; index < len(label); index++ {
			character := label[index]
			if !isS3AlphaNumeric(character) && character != '-' {
				return false
			}
		}
	}
	return true
}

func isS3AlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
}

func isLoopbackS3Host(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func (s *s3ObjectStore) objectKey(storageKey string) string {
	return s.prefix + "/" + strings.TrimLeft(storageKey, "/")
}

func (s *s3ObjectStore) requestURL(key string, query map[string]string) (*url.URL, error) {
	result := s.endpoint
	basePath := strings.TrimRight(result.Path, "/")
	objectPath := key
	if s.pathStyle {
		objectPath = s.bucket + "/" + key
	} else {
		result.Host = s.bucket + "." + result.Host
	}
	result.Path = basePath + "/" + objectPath
	result.RawPath = awsEscapePath(result.Path)
	result.RawQuery = awsCanonicalQuery(query)
	if _, err := url.Parse(result.String()); err != nil {
		return nil, err
	}
	return &result, nil
}

func awsEscapePath(value string) string {
	parts := strings.Split(value, "/")
	for index := range parts {
		parts[index] = awsEscape(parts[index])
	}
	result := strings.Join(parts, "/")
	if !strings.HasPrefix(result, "/") {
		result = "/" + result
	}
	return result
}

func awsEscape(value string) string {
	const digits = "0123456789ABCDEF"
	var output strings.Builder
	for _, valueByte := range []byte(value) {
		if valueByte >= 'a' && valueByte <= 'z' || valueByte >= 'A' && valueByte <= 'Z' || valueByte >= '0' && valueByte <= '9' || strings.ContainsRune("-_.~", rune(valueByte)) {
			output.WriteByte(valueByte)
		} else {
			output.WriteByte('%')
			output.WriteByte(digits[valueByte>>4])
			output.WriteByte(digits[valueByte&15])
		}
	}
	return output.String()
}

func awsCanonicalQuery(query map[string]string) string {
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, awsEscape(key)+"="+awsEscape(query[key]))
	}
	return strings.Join(values, "&")
}

func (s *s3ObjectStore) signedRequest(ctx context.Context, method, key string, query map[string]string, body io.Reader, payloadHash string, contentLength int64) (*http.Response, error) {
	return s.signedRequestWithHeaders(ctx, method, key, query, body, payloadHash, contentLength, nil)
}

func (s *s3ObjectStore) signedRequestWithHeaders(ctx context.Context, method, key string, query map[string]string, body io.Reader, payloadHash string, contentLength int64, extraHeaders map[string]string) (*http.Response, error) {
	if closer, ok := body.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}
	target, err := s.requestURL(key, query)
	if err != nil {
		return nil, err
	}
	now := s.clock().UTC()
	dateStamp, dateTime := now.Format("20060102"), now.Format("20060102T150405Z")
	headerValues := map[string]string{
		"host":                 target.Host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           dateTime,
	}
	if s.sessionToken != "" {
		headerValues["x-amz-security-token"] = s.sessionToken
	}
	for name, value := range extraHeaders {
		headerValues[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
	}
	headerNames := make([]string, 0, len(headerValues))
	for name := range headerValues {
		headerNames = append(headerNames, name)
	}
	sort.Strings(headerNames)
	var canonicalHeaders strings.Builder
	for _, name := range headerNames {
		canonicalHeaders.WriteString(name)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(strings.Join(strings.Fields(headerValues[name]), " "))
		canonicalHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(headerNames, ";")
	canonicalRequest := strings.Join([]string{method, target.EscapedPath(), target.RawQuery, canonicalHeaders.String(), signedHeaders, payloadHash}, "\n")
	canonicalHash := sha256.Sum256([]byte(canonicalRequest))
	scope := dateStamp + "/" + s.region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + dateTime + "\n" + scope + "\n" + hex.EncodeToString(canonicalHash[:])
	signature := s.signingKey(dateStamp, stringToSign)
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	for name, value := range headerValues {
		if name != "host" {
			request.Header.Set(name, value)
		}
	}
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.accessKeyID+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
	if body != nil && contentLength >= 0 {
		request.ContentLength = contentLength
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("S3 %s request failed: %w", method, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<10))
		closeErr := response.Body.Close()
		return nil, errors.Join(&s3HTTPError{method: method, status: response.StatusCode, body: strings.TrimSpace(string(message))}, readErr, closeErr)
	}
	return response, nil
}

func (s *s3ObjectStore) signingKey(dateStamp, stringToSign string) string {
	kDate := s3HMAC([]byte("AWS4"+s.secretAccessKey), dateStamp)
	kRegion := s3HMAC(kDate, s.region)
	kService := s3HMAC(kRegion, "s3")
	kSigning := s3HMAC(kService, "aws4_request")
	return hex.EncodeToString(s3HMAC(kSigning, stringToSign))
}

func (s *s3ObjectStore) presignedGetURL(storageKey string, expires time.Duration, responseDisposition ...string) (string, time.Time, error) {
	if expires < time.Second || expires > 7*24*time.Hour {
		return "", time.Time{}, errors.New("S3 presigned URL expiry must be between one second and seven days")
	}
	seconds := int64(expires / time.Second)
	if time.Duration(seconds)*time.Second != expires {
		return "", time.Time{}, errors.New("S3 presigned URL expiry must use whole seconds")
	}
	now := s.clock().UTC()
	dateStamp, dateTime := now.Format("20060102"), now.Format("20060102T150405Z")
	scope := dateStamp + "/" + s.region + "/s3/aws4_request"
	query := map[string]string{
		"X-Amz-Algorithm":        "AWS4-HMAC-SHA256",
		"X-Amz-Credential":       s.accessKeyID + "/" + scope,
		"X-Amz-Date":             dateTime,
		"X-Amz-Expires":          strconv.FormatInt(seconds, 10),
		"X-Amz-SignedHeaders":    "host",
		"response-cache-control": "private, no-store",
		"response-content-type":  "application/octet-stream",
	}
	if len(responseDisposition) > 0 && strings.TrimSpace(responseDisposition[0]) != "" {
		query["response-content-disposition"] = strings.TrimSpace(responseDisposition[0])
	}
	if s.sessionToken != "" {
		query["X-Amz-Security-Token"] = s.sessionToken
	}
	target, err := s.requestURL(s.objectKey(storageKey), query)
	if err != nil {
		return "", time.Time{}, err
	}
	canonicalRequest := strings.Join([]string{http.MethodGet, target.EscapedPath(), target.RawQuery, "host:" + strings.TrimSpace(target.Host) + "\n", "host", "UNSIGNED-PAYLOAD"}, "\n")
	canonicalHash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "AWS4-HMAC-SHA256\n" + dateTime + "\n" + scope + "\n" + hex.EncodeToString(canonicalHash[:])
	query["X-Amz-Signature"] = s.signingKey(dateStamp, stringToSign)
	target.RawQuery = awsCanonicalQuery(query)
	return target.String(), now.Add(time.Duration(seconds) * time.Second), nil
}

// presignedPutURL binds the capability to one upload key and the expected
// SHA-256 checksum. S3-compatible providers must validate x-amz-checksum-sha256
// against the request body before storing it.
func (s *s3ObjectStore) presignedPutURL(storageKey, expectedDigest string, expires time.Duration) (string, time.Time, string, error) {
	if !validDigest(expectedDigest) || expires < time.Second || expires > 15*time.Minute || expires%time.Second != 0 {
		return "", time.Time{}, "", errors.New("S3 direct upload requires a valid SHA-256 and expiry between one second and fifteen minutes")
	}
	now := s.clock().UTC()
	dateStamp, dateTime := now.Format("20060102"), now.Format("20060102T150405Z")
	scope := dateStamp + "/" + s.region + "/s3/aws4_request"
	checksumBytes, err := hex.DecodeString(expectedDigest)
	if err != nil {
		return "", time.Time{}, "", err
	}
	checksum := base64.StdEncoding.EncodeToString(checksumBytes)
	query := map[string]string{
		"X-Amz-Algorithm":     "AWS4-HMAC-SHA256",
		"X-Amz-Credential":    s.accessKeyID + "/" + scope,
		"X-Amz-Date":          dateTime,
		"X-Amz-Expires":       strconv.FormatInt(int64(expires/time.Second), 10),
		"X-Amz-SignedHeaders": "host;x-amz-checksum-sha256",
	}
	if s.sessionToken != "" {
		query["X-Amz-Security-Token"] = s.sessionToken
	}
	target, err := s.requestURL(s.objectKey(storageKey), query)
	if err != nil {
		return "", time.Time{}, "", err
	}
	canonicalHeaders := "host:" + strings.TrimSpace(target.Host) + "\nx-amz-checksum-sha256:" + checksum + "\n"
	canonicalRequest := strings.Join([]string{http.MethodPut, target.EscapedPath(), target.RawQuery, canonicalHeaders, "host;x-amz-checksum-sha256", "UNSIGNED-PAYLOAD"}, "\n")
	canonicalHash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "AWS4-HMAC-SHA256\n" + dateTime + "\n" + scope + "\n" + hex.EncodeToString(canonicalHash[:])
	query["X-Amz-Signature"] = s.signingKey(dateStamp, stringToSign)
	target.RawQuery = awsCanonicalQuery(query)
	return target.String(), now.Add(expires), checksum, nil
}

func (s *s3ObjectStore) headObject(ctx context.Context, storageKey string) (int64, string, error) {
	response, err := s.signedRequest(ctx, http.MethodHead, s.objectKey(storageKey), nil, nil, emptySHA256, 0)
	if err != nil {
		var statusError *s3HTTPError
		if errors.As(err, &statusError) && statusError.status == http.StatusNotFound {
			return 0, "", errors.Join(errArtifactRemoteNotFound, err)
		}
		return 0, "", err
	}
	size := response.ContentLength
	etag := strings.TrimSpace(response.Header.Get("ETag"))
	closeErr := response.Body.Close()
	if size < 0 || etag == "" || closeErr != nil {
		return 0, "", errors.Join(fmt.Errorf("S3 HEAD did not return a stable object size and ETag: %w", errArtifactRemoteMismatch), closeErr)
	}
	return size, etag, nil
}

func (s *s3ObjectStore) copyObject(ctx context.Context, sourceKey, targetKey, sourceETag string) error {
	if strings.TrimSpace(sourceETag) == "" {
		return domain.ErrInvalid
	}
	copySource := awsEscapePath("/" + s.bucket + "/" + s.objectKey(sourceKey))
	response, err := s.signedRequestWithHeaders(ctx, http.MethodPut, s.objectKey(targetKey), nil, nil, emptySHA256, 0, map[string]string{
		"x-amz-copy-source":          copySource,
		"x-amz-copy-source-if-match": sourceETag,
		"x-amz-metadata-directive":   "COPY",
	})
	if err != nil {
		return err
	}
	var result struct {
		Error *struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		} `xml:"Error"`
	}
	decodeErr := xml.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result)
	closeErr := response.Body.Close()
	if decodeErr != nil || closeErr != nil || result.Error != nil {
		var embedded error
		if result.Error != nil {
			embedded = fmt.Errorf("S3 copy failed: %s: %s", result.Error.Code, result.Error.Message)
		}
		return errors.Join(errors.New("S3 conditional object copy did not complete"), decodeErr, closeErr, embedded)
	}
	return nil
}

func (s *s3ObjectStore) deleteObject(ctx context.Context, storageKey string) error {
	response, err := s.signedRequest(ctx, http.MethodDelete, s.objectKey(storageKey), nil, nil, emptySHA256, 0)
	if err != nil {
		var statusError *s3HTTPError
		if errors.As(err, &statusError) && statusError.status == http.StatusNotFound {
			return nil
		}
		return err
	}
	return response.Body.Close()
}

func s3HMAC(key []byte, value string) []byte {
	hasher := hmac.New(sha256.New, key)
	_, _ = io.WriteString(hasher, value)
	return hasher.Sum(nil)
}

func (s *s3ObjectStore) putObject(ctx context.Context, storageKey, filePath string, size int64) error {
	key := s.objectKey(storageKey)
	if size <= s.multipartLimit {
		file, err := os.Open(filePath)
		if err != nil {
			return err
		}
		hash, hashedSize, hashErr := hashReader(file)
		if hashErr != nil || hashedSize != size {
			return errors.Join(errors.New("S3 upload source changed before upload"), hashErr, file.Close())
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return errors.Join(err, file.Close())
		}
		response, err := s.signedRequest(ctx, http.MethodPut, key, nil, file, hash, size)
		if err != nil {
			return err
		}
		return response.Body.Close()
	}
	return s.putMultipart(ctx, key, filePath, size)
}

type s3CompletedPart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

func (s *s3ObjectStore) putMultipart(ctx context.Context, key, filePath string, size int64) (retErr error) {
	partSize := s3MinPartSize
	if size/s3MaxParts > partSize {
		partSize = size / s3MaxParts
		if size%s3MaxParts != 0 {
			partSize++
		}
		partSize = ((partSize + s3MinPartSize - 1) / s3MinPartSize) * s3MinPartSize
	}
	if partSize > s3MaxPartSize || (size+partSize-1)/partSize > s3MaxParts {
		return errors.New("S3-compatible multipart limits cannot store this artifact size")
	}
	createResponse, err := s.signedRequest(ctx, http.MethodPost, key, map[string]string{"uploads": ""}, nil, emptySHA256, 0)
	if err != nil {
		return err
	}
	var created struct {
		UploadID string `xml:"UploadId"`
	}
	decodeErr := xml.NewDecoder(io.LimitReader(createResponse.Body, 1<<20)).Decode(&created)
	closeErr := createResponse.Body.Close()
	if decodeErr != nil || closeErr != nil || strings.TrimSpace(created.UploadID) == "" {
		return errors.Join(errors.New("S3 multipart initiation did not return an upload ID"), decodeErr, closeErr)
	}
	completed := false
	defer func() {
		if !completed {
			abortCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			abortResponse, abortErr := s.signedRequest(abortCtx, http.MethodDelete, key, map[string]string{"uploadId": created.UploadID}, nil, emptySHA256, 0)
			if abortErr == nil {
				abortErr = abortResponse.Body.Close()
			}
			retErr = errors.Join(retErr, wrapS3AbortError(abortErr))
		}
	}()
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	parts := make([]s3CompletedPart, 0, (size+partSize-1)/partSize)
	for offset, partNumber := int64(0), 1; offset < size; partNumber++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		length := partSize
		if remaining := size - offset; remaining < length {
			length = remaining
		}
		section := io.NewSectionReader(file, offset, length)
		partHash := sha256.New()
		if _, err := io.Copy(partHash, section); err != nil {
			return err
		}
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		response, err := s.signedRequest(ctx, http.MethodPut, key, map[string]string{"partNumber": strconv.Itoa(partNumber), "uploadId": created.UploadID}, io.NewSectionReader(file, offset, length), hex.EncodeToString(partHash.Sum(nil)), length)
		if err != nil {
			return err
		}
		etag := strings.TrimSpace(response.Header.Get("ETag"))
		closeErr := response.Body.Close()
		if etag == "" || closeErr != nil {
			return errors.Join(fmt.Errorf("S3 multipart part %d did not return a durable ETag", partNumber), closeErr)
		}
		parts = append(parts, s3CompletedPart{PartNumber: partNumber, ETag: etag})
		offset += length
	}
	completion := struct {
		XMLName xml.Name          `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CompleteMultipartUpload"`
		Parts   []s3CompletedPart `xml:"Part"`
	}{Parts: parts}
	completionBody, err := xml.Marshal(completion)
	if err != nil {
		return err
	}
	completionHash := sha256.Sum256(completionBody)
	response, err := s.signedRequest(ctx, http.MethodPost, key, map[string]string{"uploadId": created.UploadID}, strings.NewReader(string(completionBody)), hex.EncodeToString(completionHash[:]), int64(len(completionBody)))
	if err != nil {
		return err
	}
	var completedResult struct {
		Error *struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		} `xml:"Error"`
	}
	decodeErr = xml.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&completedResult)
	closeErr = response.Body.Close()
	if decodeErr != nil || closeErr != nil || completedResult.Error != nil {
		var embeddedErr error
		if completedResult.Error != nil {
			embeddedErr = fmt.Errorf("S3 multipart completion failed: %s: %s", completedResult.Error.Code, completedResult.Error.Message)
		}
		return errors.Join(errors.New("S3 multipart completion was not verified"), decodeErr, closeErr, embeddedErr)
	}
	completed = true
	return nil
}

func wrapS3AbortError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("abort incomplete S3 multipart upload: %w", err)
}

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (s *s3ObjectStore) openObject(ctx context.Context, storageKey string) (io.ReadCloser, error) {
	response, err := s.signedRequest(ctx, http.MethodGet, s.objectKey(storageKey), nil, nil, emptySHA256, 0)
	if err != nil {
		var statusError *s3HTTPError
		if errors.As(err, &statusError) && statusError.status == http.StatusNotFound {
			return nil, errors.Join(errArtifactRemoteNotFound, err)
		}
		return nil, err
	}
	return response.Body, nil
}

func (s *s3ObjectStore) verifyObjectHead(ctx context.Context, storageKey string, expectedSize int64) error {
	response, err := s.signedRequest(ctx, http.MethodHead, s.objectKey(storageKey), nil, nil, emptySHA256, 0)
	if err != nil {
		var statusError *s3HTTPError
		if errors.As(err, &statusError) && statusError.status == http.StatusNotFound {
			return errors.Join(errArtifactRemoteNotFound, err)
		}
		return err
	}
	closeErr := response.Body.Close()
	if response.ContentLength != expectedSize || closeErr != nil {
		return errors.Join(fmt.Errorf("S3 object HEAD did not match the committed artifact size: %w", errArtifactRemoteMismatch), closeErr)
	}
	return nil
}

var errArtifactRemoteMismatch = errors.New("remote artifact object mismatch")
var errArtifactRemoteNotFound = errors.New("remote artifact object not found")
