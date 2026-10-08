package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
)

type Config struct {
	Addr                      string
	DataDir                   string
	WorkspaceRoot             string
	AgentTempDir              string
	FrontendOrigin            string
	AuthBootstrapToken        string
	AuthOwnerEmail            string
	ResendAPIKey              string
	AuthMailFrom              string
	TurnstileSiteKey          string
	TurnstileSecretKey        string
	TLSCertFile               string
	TLSKeyFile                string
	AgentMaxModelCalls        int
	CloudWorkerConcurrency    int
	NodeWorkerConcurrency     int
	ExecutionRole             string
	NodeControlURL            string
	NodeCredential            string
	NodeProviderID            string
	QuotaHelperSocket         string
	CloudWorkspaceQuota       int64
	CloudWorkspaceDiskReserve int64
	CloudWorkspaceRetention   time.Duration
	ArtifactStoreBackend      string
	ArtifactS3Endpoint        string
	ArtifactS3Region          string
	ArtifactS3Bucket          string
	ArtifactS3AccessKeyID     string
	ArtifactS3SecretKey       string
	ArtifactS3SessionToken    string
	ArtifactS3Prefix          string
	ArtifactS3PathStyle       bool
	ArtifactS3DirectUpload    bool
	LoadError                 error
}

func Load() Config {
	config := Config{
		Addr:                      env("O_ADDR", "AXIOM_ADDR", "127.0.0.1:9171"),
		DataDir:                   env("O_DATA_DIR", "AXIOM_DATA_DIR", filepath.Join("..", "data")),
		WorkspaceRoot:             env("O_WORKSPACE_ROOT", "AXIOM_WORKSPACE_ROOT", filepath.Clean("..")),
		AgentTempDir:              env("O_AGENT_TEMP_DIR", "AXIOM_AGENT_TEMP_DIR", filepath.Join(os.TempDir(), "Axiom", "agent-runs")),
		FrontendOrigin:            env("O_FRONTEND_ORIGIN", "AXIOM_FRONTEND_ORIGIN", "http://127.0.0.1:3000"),
		AuthBootstrapToken:        env("O_AUTH_BOOTSTRAP_TOKEN", "AXIOM_AUTH_BOOTSTRAP_TOKEN", ""),
		AuthOwnerEmail:            env("O_AUTH_OWNER_EMAIL", "", ""),
		ResendAPIKey:              env("O_RESEND_API_KEY", "", ""),
		AuthMailFrom:              env("O_AUTH_MAIL_FROM", "", ""),
		TurnstileSiteKey:          env("O_TURNSTILE_SITE_KEY", "", ""),
		TurnstileSecretKey:        env("O_TURNSTILE_SECRET_KEY", "", ""),
		TLSCertFile:               env("O_TLS_CERT_FILE", "AXIOM_TLS_CERT_FILE", ""),
		TLSKeyFile:                env("O_TLS_KEY_FILE", "AXIOM_TLS_KEY_FILE", ""),
		AgentMaxModelCalls:        0,
		CloudWorkerConcurrency:    domain.MaxCloudWorkerConcurrency,
		NodeWorkerConcurrency:     domain.MaxLocalNodeWorkerConcurrency,
		ExecutionRole:             env("O_EXECUTION_ROLE", "AXIOM_EXECUTION_ROLE", "local"),
		NodeControlURL:            env("O_NODE_CONTROL_URL", "AXIOM_NODE_CONTROL_URL", ""),
		NodeCredential:            env("O_NODE_CREDENTIAL", "AXIOM_NODE_CREDENTIAL", ""),
		NodeProviderID:            env("O_NODE_PROVIDER_ID", "AXIOM_NODE_PROVIDER_ID", ""),
		QuotaHelperSocket:         env("O_QUOTA_HELPER_SOCKET", "AXIOM_QUOTA_HELPER_SOCKET", ""),
		CloudWorkspaceQuota:       0,
		CloudWorkspaceDiskReserve: domain.CloudWorkspaceDiskReserveBytes,
		CloudWorkspaceRetention:   30 * 24 * time.Hour,
		ArtifactStoreBackend:      env("O_ARTIFACT_STORE_BACKEND", "AXIOM_ARTIFACT_STORE_BACKEND", "local"),
		ArtifactS3Endpoint:        env("O_ARTIFACT_S3_ENDPOINT", "AXIOM_ARTIFACT_S3_ENDPOINT", ""),
		ArtifactS3Region:          env("O_ARTIFACT_S3_REGION", "AXIOM_ARTIFACT_S3_REGION", ""),
		ArtifactS3Bucket:          env("O_ARTIFACT_S3_BUCKET", "AXIOM_ARTIFACT_S3_BUCKET", ""),
		ArtifactS3AccessKeyID:     env("O_ARTIFACT_S3_ACCESS_KEY_ID", "AXIOM_ARTIFACT_S3_ACCESS_KEY_ID", ""),
		ArtifactS3SecretKey:       env("O_ARTIFACT_S3_SECRET_ACCESS_KEY", "AXIOM_ARTIFACT_S3_SECRET_ACCESS_KEY", ""),
		ArtifactS3SessionToken:    env("O_ARTIFACT_S3_SESSION_TOKEN", "AXIOM_ARTIFACT_S3_SESSION_TOKEN", ""),
		ArtifactS3Prefix:          env("O_ARTIFACT_S3_PREFIX", "AXIOM_ARTIFACT_S3_PREFIX", "o-agent/artifacts"),
		ArtifactS3PathStyle:       true,
		ArtifactS3DirectUpload:    false,
	}
	if value := env("O_ARTIFACT_S3_PATH_STYLE", "AXIOM_ARTIFACT_S3_PATH_STYLE", "true"); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			config.LoadError = fmt.Errorf("parse O_ARTIFACT_S3_PATH_STYLE: %w", err)
		} else {
			config.ArtifactS3PathStyle = parsed
		}
	}
	if value := env("O_ARTIFACT_S3_DIRECT_UPLOAD", "AXIOM_ARTIFACT_S3_DIRECT_UPLOAD", "false"); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			config.LoadError = fmt.Errorf("parse O_ARTIFACT_S3_DIRECT_UPLOAD: %w", err)
		} else {
			config.ArtifactS3DirectUpload = parsed
		}
	}
	if value, err := intSetting("O_AGENT_MAX_MODEL_CALLS", "AXIOM_AGENT_MAX_MODEL_CALLS", config.AgentMaxModelCalls); err != nil {
		config.LoadError = err
	} else {
		config.AgentMaxModelCalls = value
	}
	if value, err := intSetting("O_CLOUD_WORKER_CONCURRENCY", "AXIOM_CLOUD_WORKER_CONCURRENCY", config.CloudWorkerConcurrency); err != nil {
		config.LoadError = err
	} else {
		config.CloudWorkerConcurrency = value
	}
	if value, err := intSetting("O_NODE_WORKER_CONCURRENCY", "AXIOM_NODE_WORKER_CONCURRENCY", config.NodeWorkerConcurrency); err != nil {
		config.LoadError = err
	} else {
		config.NodeWorkerConcurrency = value
	}
	if value, err := int64Setting("O_CLOUD_TASK_WORKSPACE_QUOTA_BYTES", "AXIOM_CLOUD_TASK_WORKSPACE_QUOTA_BYTES", config.CloudWorkspaceQuota); err != nil {
		config.LoadError = err
	} else {
		config.CloudWorkspaceQuota = value
	}
	if value, err := int64Setting("O_CLOUD_TASK_DISK_RESERVE_BYTES", "AXIOM_CLOUD_TASK_DISK_RESERVE_BYTES", config.CloudWorkspaceDiskReserve); err != nil {
		config.LoadError = err
	} else {
		config.CloudWorkspaceDiskReserve = value
	}
	if value := env("O_CLOUD_TASK_WORKSPACE_RETENTION", "AXIOM_CLOUD_TASK_WORKSPACE_RETENTION", ""); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil {
			config.LoadError = fmt.Errorf("parse O_CLOUD_TASK_WORKSPACE_RETENTION: %w", err)
		} else {
			config.CloudWorkspaceRetention = duration
		}
	}
	return config
}

func (c Config) Validate() error {
	if c.LoadError != nil {
		return c.LoadError
	}
	if (c.ResendAPIKey == "") != (c.AuthMailFrom == "") {
		return fmt.Errorf("O_RESEND_API_KEY and O_AUTH_MAIL_FROM must be configured together")
	}
	if (c.TurnstileSiteKey == "") != (c.TurnstileSecretKey == "") {
		return fmt.Errorf("O_TURNSTILE_SITE_KEY and O_TURNSTILE_SECRET_KEY must be configured together")
	}
	if c.AgentMaxModelCalls < 0 {
		return fmt.Errorf("O_AGENT_MAX_MODEL_CALLS must be 0 (unlimited) or greater")
	}
	if c.CloudWorkerConcurrency < 1 || c.CloudWorkerConcurrency > domain.MaxCloudWorkerConcurrency {
		return fmt.Errorf("O_CLOUD_WORKER_CONCURRENCY must be between 1 and %d; live host admission selects the active count", domain.MaxCloudWorkerConcurrency)
	}
	if c.NodeWorkerConcurrency < 1 || c.NodeWorkerConcurrency > domain.MaxLocalNodeWorkerConcurrency {
		return fmt.Errorf("O_NODE_WORKER_CONCURRENCY must be between 1 and %d; live host admission selects the active count", domain.MaxLocalNodeWorkerConcurrency)
	}
	if c.ExecutionRole != "local" && c.ExecutionRole != "cloud" {
		return fmt.Errorf("O_EXECUTION_ROLE must be local or cloud")
	}
	if c.CloudWorkspaceQuota < 0 {
		return fmt.Errorf("O_CLOUD_TASK_WORKSPACE_QUOTA_BYTES must be a positive byte count for cloud role")
	}
	if c.CloudWorkspaceDiskReserve < 0 {
		return fmt.Errorf("O_CLOUD_TASK_DISK_RESERVE_BYTES must be zero or a positive byte count")
	}
	if c.CloudWorkspaceRetention < 0 {
		return fmt.Errorf("O_CLOUD_TASK_WORKSPACE_RETENTION must be zero (disabled) or a positive duration")
	}
	if c.ArtifactStoreBackend != "local" && c.ArtifactStoreBackend != "s3" {
		return fmt.Errorf("O_ARTIFACT_STORE_BACKEND must be local or s3")
	}
	if c.ArtifactS3DirectUpload && c.ArtifactStoreBackend != "s3" {
		return fmt.Errorf("O_ARTIFACT_S3_DIRECT_UPLOAD requires O_ARTIFACT_STORE_BACKEND=s3")
	}
	if c.ArtifactStoreBackend == "s3" {
		endpoint, err := url.Parse(c.ArtifactS3Endpoint)
		if err != nil || !endpoint.IsAbs() || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
			return fmt.Errorf("O_ARTIFACT_S3_ENDPOINT must be an absolute HTTP(S) URL without credentials, query, or fragment")
		}
		if endpoint.Scheme != "https" && !isLoopbackHost(endpoint.Hostname()) {
			return fmt.Errorf("O_ARTIFACT_S3_ENDPOINT must use HTTPS outside loopback testing")
		}
		bucket := c.ArtifactS3Bucket
		if !validArtifactS3BucketName(bucket) {
			return fmt.Errorf("O_ARTIFACT_S3_BUCKET must be a valid bucket name")
		}
		if strings.TrimSpace(c.ArtifactS3Region) == "" || strings.TrimSpace(c.ArtifactS3AccessKeyID) == "" || strings.TrimSpace(c.ArtifactS3SecretKey) == "" {
			return fmt.Errorf("O_ARTIFACT_S3_REGION, O_ARTIFACT_S3_ACCESS_KEY_ID and O_ARTIFACT_S3_SECRET_ACCESS_KEY are required for the s3 backend")
		}
		prefix := strings.TrimSpace(c.ArtifactS3Prefix)
		if prefix == "" || strings.HasPrefix(prefix, "/") || strings.ContainsAny(prefix, "\\:\x00\r\n") || path.Clean(prefix) != prefix || prefix == "." || prefix == ".." || strings.HasPrefix(prefix, "../") {
			return fmt.Errorf("O_ARTIFACT_S3_PREFIX must be a normalized relative key prefix")
		}
	}
	if c.ExecutionRole == "cloud" && c.CloudWorkspaceQuota == 0 {
		return fmt.Errorf("O_CLOUD_TASK_WORKSPACE_QUOTA_BYTES must configure the cloud task hard disk quota")
	}
	if c.ExecutionRole == "cloud" && (c.QuotaHelperSocket == "" || !path.IsAbs(c.QuotaHelperSocket) || path.Clean(c.QuotaHelperSocket) != c.QuotaHelperSocket) {
		return fmt.Errorf("O_QUOTA_HELPER_SOCKET must be a canonical absolute Unix socket path for cloud role")
	}
	if c.ExecutionRole == "local" && (c.CloudWorkspaceQuota != 0 || c.QuotaHelperSocket != "") {
		return fmt.Errorf("cloud task workspace quota settings require O_EXECUTION_ROLE=cloud")
	}
	frontendOrigin, err := url.Parse(c.FrontendOrigin)
	if err != nil || !frontendOrigin.IsAbs() || (frontendOrigin.Scheme != "https" && frontendOrigin.Scheme != "http") || frontendOrigin.Hostname() == "" || frontendOrigin.User != nil || frontendOrigin.RawQuery != "" || frontendOrigin.Fragment != "" || (frontendOrigin.Path != "" && frontendOrigin.Path != "/") {
		return fmt.Errorf("O_FRONTEND_ORIGIN must be an HTTP(S) origin without credentials, query, fragment, or path")
	}
	publicFrontend := !isLoopbackHost(frontendOrigin.Hostname())
	if c.ExecutionRole == "cloud" && frontendOrigin.Scheme != "https" {
		return fmt.Errorf("O_FRONTEND_ORIGIN must use HTTPS when O_EXECUTION_ROLE=cloud")
	}
	if (c.ExecutionRole == "cloud" || publicFrontend) && len(c.AuthBootstrapToken) < 32 {
		return fmt.Errorf("O_AUTH_BOOTSTRAP_TOKEN must contain at least 32 characters for a cloud role or public frontend origin")
	}
	if publicFrontend && frontendOrigin.Scheme != "https" {
		return fmt.Errorf("O_FRONTEND_ORIGIN must use HTTPS when it is not loopback")
	}
	nodeWorkerConfigured := c.NodeControlURL != "" || c.NodeCredential != ""
	if nodeWorkerConfigured && (c.NodeControlURL == "" || c.NodeCredential == "") {
		return fmt.Errorf("O_NODE_CONTROL_URL and O_NODE_CREDENTIAL must be configured together")
	}
	if nodeWorkerConfigured {
		if len(c.NodeCredential) < 32 {
			return fmt.Errorf("O_NODE_CREDENTIAL must contain at least 32 characters")
		}
		controlURL, err := url.Parse(c.NodeControlURL)
		if err != nil || !controlURL.IsAbs() || controlURL.Hostname() == "" || controlURL.User != nil || controlURL.RawQuery != "" || controlURL.Fragment != "" || (controlURL.Scheme != "https" && controlURL.Scheme != "http") {
			return fmt.Errorf("O_NODE_CONTROL_URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
		}
		if controlURL.Scheme != "https" && !isLoopbackHost(controlURL.Hostname()) {
			return fmt.Errorf("O_NODE_CONTROL_URL must use HTTPS outside loopback testing")
		}
	}
	if !nodeWorkerConfigured && c.NodeProviderID != "" {
		return fmt.Errorf("O_NODE_PROVIDER_ID requires an active O_NODE_CONTROL_URL and O_NODE_CREDENTIAL")
	}
	if c.ExecutionRole == "cloud" && nodeWorkerConfigured {
		return fmt.Errorf("O_NODE_CONTROL_URL cannot be used with O_EXECUTION_ROLE=cloud")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return fmt.Errorf("O_TLS_CERT_FILE and O_TLS_KEY_FILE must be configured together")
	}
	if c.AuthBootstrapToken != "" && len(c.AuthBootstrapToken) < 32 {
		return fmt.Errorf("O_AUTH_BOOTSTRAP_TOKEN must contain at least 32 characters when authentication is enabled")
	}
	if !isLoopbackBind(c.Addr) {
		if c.AuthBootstrapToken == "" {
			return fmt.Errorf("O_AUTH_BOOTSTRAP_TOKEN must contain at least 32 characters before binding O_ADDR to a non-loopback interface")
		}
		if c.TLSCertFile == "" || c.TLSKeyFile == "" {
			return fmt.Errorf("O_TLS_CERT_FILE and O_TLS_KEY_FILE are required for a non-loopback listener")
		}
	}
	return nil
}

func validArtifactS3BucketName(bucket string) bool {
	if len(bucket) < 3 || len(bucket) > 63 || net.ParseIP(bucket) != nil || strings.Contains(bucket, "..") {
		return false
	}
	for _, label := range strings.Split(bucket, ".") {
		if label == "" || !isArtifactS3AlphaNumeric(label[0]) || !isArtifactS3AlphaNumeric(label[len(label)-1]) {
			return false
		}
		for index := 0; index < len(label); index++ {
			character := label[index]
			if !isArtifactS3AlphaNumeric(character) && character != '-' {
				return false
			}
		}
	}
	return true
}

func isArtifactS3AlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
}

func isLoopbackBind(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func intSetting(name, legacyName string, fallback int) (int, error) {
	value := env(name, legacyName, "")
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed, nil
}

func int64Setting(name, legacyName string, fallback int64) (int64, error) {
	value := env(name, legacyName, "")
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed, nil
}

func env(name, legacyName, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	if value := os.Getenv(legacyName); value != "" {
		return value
	}
	return fallback
}
