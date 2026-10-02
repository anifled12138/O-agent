package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
)

func TestRemoteBindRequiresBootstrapCredential(t *testing.T) {
	for _, name := range []string{"O_ADDR", "AXIOM_ADDR", "O_AUTH_BOOTSTRAP_TOKEN", "AXIOM_AUTH_BOOTSTRAP_TOKEN", "O_TLS_CERT_FILE", "AXIOM_TLS_CERT_FILE", "O_TLS_KEY_FILE", "AXIOM_TLS_KEY_FILE", "O_AGENT_MAX_MODEL_CALLS", "AXIOM_AGENT_MAX_MODEL_CALLS"} {
		t.Setenv(name, "")
	}
	t.Setenv("O_ADDR", "0.0.0.0:9171")
	if err := Load().Validate(); err == nil {
		t.Fatal("public bind started without an authentication bootstrap secret")
	}
	t.Setenv("O_AUTH_BOOTSTRAP_TOKEN", "01234567890123456789012345678901")
	if err := Load().Validate(); err == nil {
		t.Fatal("public bind started without TLS")
	}
	t.Setenv("O_TLS_CERT_FILE", "server.crt")
	t.Setenv("O_TLS_KEY_FILE", "server.key")
	if err := Load().Validate(); err != nil {
		t.Fatalf("public bind with strong bootstrap secret failed: %v", err)
	}
	t.Setenv("O_ADDR", "127.0.0.1:9171")
	t.Setenv("O_AUTH_BOOTSTRAP_TOKEN", "")
	t.Setenv("O_TLS_CERT_FILE", "")
	t.Setenv("O_TLS_KEY_FILE", "")
	if err := Load().Validate(); err != nil {
		t.Fatalf("loopback-only default should remain usable: %v", err)
	}
	t.Setenv("O_AUTH_BOOTSTRAP_TOKEN", "short")
	if err := Load().Validate(); err == nil {
		t.Fatal("authentication accepted a short bootstrap credential")
	}
}

func TestCloudWorkerConcurrencyIsAHostAdaptiveCeiling(t *testing.T) {
	for _, name := range []string{"O_CLOUD_WORKER_CONCURRENCY", "AXIOM_CLOUD_WORKER_CONCURRENCY"} {
		t.Setenv(name, "")
	}
	if got := Load().CloudWorkerConcurrency; got != domain.MaxCloudWorkerConcurrency {
		t.Fatalf("default cloud worker ceiling = %d, want %d", got, domain.MaxCloudWorkerConcurrency)
	}
	t.Setenv("O_CLOUD_WORKER_CONCURRENCY", "12")
	if cfg := Load(); cfg.CloudWorkerConcurrency != 12 || cfg.Validate() != nil {
		t.Fatalf("host worker ceiling configuration was rejected: %+v, err=%v", cfg, cfg.Validate())
	}
	t.Setenv("O_CLOUD_WORKER_CONCURRENCY", "65")
	if err := Load().Validate(); err == nil {
		t.Fatal("cloud worker ceiling above the supported scheduler capacity was accepted")
	}
}

func TestS3ArtifactBackendRequiresSecureCompleteConfiguration(t *testing.T) {
	for _, name := range []string{
		"O_ARTIFACT_STORE_BACKEND", "AXIOM_ARTIFACT_STORE_BACKEND", "O_ARTIFACT_S3_ENDPOINT", "AXIOM_ARTIFACT_S3_ENDPOINT",
		"O_ARTIFACT_S3_REGION", "AXIOM_ARTIFACT_S3_REGION", "O_ARTIFACT_S3_BUCKET", "AXIOM_ARTIFACT_S3_BUCKET",
		"O_ARTIFACT_S3_ACCESS_KEY_ID", "AXIOM_ARTIFACT_S3_ACCESS_KEY_ID", "O_ARTIFACT_S3_SECRET_ACCESS_KEY", "AXIOM_ARTIFACT_S3_SECRET_ACCESS_KEY",
		"O_ARTIFACT_S3_SESSION_TOKEN", "AXIOM_ARTIFACT_S3_SESSION_TOKEN", "O_ARTIFACT_S3_PREFIX", "AXIOM_ARTIFACT_S3_PREFIX",
		"O_ARTIFACT_S3_PATH_STYLE", "AXIOM_ARTIFACT_S3_PATH_STYLE", "O_ARTIFACT_S3_DIRECT_UPLOAD", "AXIOM_ARTIFACT_S3_DIRECT_UPLOAD",
	} {
		t.Setenv(name, "")
	}
	config := Load()
	if config.ArtifactStoreBackend != "local" || config.Validate() != nil {
		t.Fatalf("local artifact storage default changed: %+v err=%v", config, config.Validate())
	}
	config.ArtifactStoreBackend = "s3"
	config.ArtifactS3Endpoint = "https://objects.example.test"
	config.ArtifactS3Region = "us-east-1"
	config.ArtifactS3Bucket = "o-agent-artifacts"
	config.ArtifactS3AccessKeyID = "access"
	config.ArtifactS3SecretKey = "secret"
	if err := config.Validate(); err != nil {
		t.Fatalf("complete HTTPS S3 configuration was rejected: %v", err)
	}
	if config.ArtifactS3DirectUpload {
		t.Fatal("direct upload must be opt-in until the provider's checksum and conditional-copy behavior is verified")
	}
	t.Setenv("O_ARTIFACT_S3_DIRECT_UPLOAD", "true")
	if cfg := Load(); !cfg.ArtifactS3DirectUpload {
		t.Fatalf("verified S3 direct upload opt-in was not loaded: %+v", cfg)
	}
	config.ArtifactS3DirectUpload = true
	config.ArtifactS3Endpoint = "http://objects.example.test"
	if err := config.Validate(); err == nil {
		t.Fatal("remote S3 endpoint without HTTPS was accepted")
	}
	config.ArtifactS3Endpoint = "http://127.0.0.1:9000"
	if err := config.Validate(); err != nil {
		t.Fatalf("loopback S3 endpoint for development testing was rejected: %v", err)
	}
	config.ArtifactS3Endpoint = "http://LOCALHOST:9000"
	if err := config.Validate(); err != nil {
		t.Fatalf("case-insensitive loopback S3 endpoint was rejected: %v", err)
	}
	config.ArtifactS3Endpoint = "https://objects.example.test"
	config.ArtifactS3Prefix = "o-agent/a..b"
	if err := config.Validate(); err != nil {
		t.Fatalf("valid relative S3 prefix containing adjacent dots was rejected: %v", err)
	}
	config.ArtifactS3SecretKey = ""
	if err := config.Validate(); err == nil {
		t.Fatal("S3 backend without a secret key was accepted")
	}
	config.ArtifactS3SecretKey = "secret"
	config.ArtifactS3Prefix = "../escape"
	if err := config.Validate(); err == nil {
		t.Fatal("S3 object key prefix escaping the configured namespace was accepted")
	}
	config.ArtifactStoreBackend = "local"
	config.ArtifactS3Prefix = "o-agent/artifacts"
	if err := config.Validate(); err == nil {
		t.Fatal("direct upload setting was accepted without the S3 artifact backend")
	}
}

func TestCloudWorkspaceRetentionDefaultsAndCanBeDisabled(t *testing.T) {
	for _, name := range []string{"O_CLOUD_TASK_WORKSPACE_RETENTION", "AXIOM_CLOUD_TASK_WORKSPACE_RETENTION"} {
		t.Setenv(name, "")
	}
	if cfg := Load(); cfg.CloudWorkspaceRetention != 30*24*time.Hour || cfg.Validate() != nil {
		t.Fatalf("default cloud workspace retention = %s, validation error = %v", cfg.CloudWorkspaceRetention, cfg.Validate())
	}
	t.Setenv("O_CLOUD_TASK_WORKSPACE_RETENTION", "0")
	if cfg := Load(); cfg.CloudWorkspaceRetention != 0 || cfg.Validate() != nil {
		t.Fatalf("disabled workspace retention was rejected: %s, err=%v", cfg.CloudWorkspaceRetention, cfg.Validate())
	}
	t.Setenv("O_CLOUD_TASK_WORKSPACE_RETENTION", "not-a-duration")
	if err := Load().Validate(); err == nil {
		t.Fatal("invalid workspace retention duration was accepted")
	}
	t.Setenv("O_CLOUD_TASK_WORKSPACE_RETENTION", "-1h")
	if err := Load().Validate(); err == nil {
		t.Fatal("negative workspace retention duration was accepted")
	}
}

func TestCloudRoleRequiresAuthenticatedHTTPSFrontendBehindLoopbackProxy(t *testing.T) {
	for _, name := range []string{"O_ADDR", "AXIOM_ADDR", "O_AUTH_BOOTSTRAP_TOKEN", "AXIOM_AUTH_BOOTSTRAP_TOKEN", "O_TLS_CERT_FILE", "AXIOM_TLS_CERT_FILE", "O_TLS_KEY_FILE", "AXIOM_TLS_KEY_FILE", "O_FRONTEND_ORIGIN", "AXIOM_FRONTEND_ORIGIN", "O_EXECUTION_ROLE", "AXIOM_EXECUTION_ROLE", "O_NODE_CONTROL_URL", "AXIOM_NODE_CONTROL_URL", "O_NODE_CREDENTIAL", "AXIOM_NODE_CREDENTIAL", "O_QUOTA_HELPER_SOCKET", "AXIOM_QUOTA_HELPER_SOCKET", "O_CLOUD_TASK_WORKSPACE_QUOTA_BYTES", "AXIOM_CLOUD_TASK_WORKSPACE_QUOTA_BYTES", "O_CLOUD_TASK_DISK_RESERVE_BYTES", "AXIOM_CLOUD_TASK_DISK_RESERVE_BYTES"} {
		t.Setenv(name, "")
	}
	t.Setenv("O_EXECUTION_ROLE", "cloud")
	t.Setenv("O_ADDR", "127.0.0.1:9171")
	if err := Load().Validate(); err == nil {
		t.Fatal("loopback cloud API without an authentication bootstrap token was accepted")
	}
	t.Setenv("O_AUTH_BOOTSTRAP_TOKEN", "01234567890123456789012345678901")
	if err := Load().Validate(); err == nil {
		t.Fatal("cloud API with an insecure default frontend origin was accepted")
	}
	t.Setenv("O_FRONTEND_ORIGIN", "https://o.example.test")
	if err := Load().Validate(); err == nil {
		t.Fatal("cloud role started without a durable task hard quota and root helper socket")
	}
	t.Setenv("O_QUOTA_HELPER_SOCKET", "/run/o-agent/quota.sock")
	t.Setenv("O_CLOUD_TASK_WORKSPACE_QUOTA_BYTES", "8589934592")
	if cfg := Load(); cfg.CloudWorkspaceDiskReserve != domain.CloudWorkspaceDiskReserveBytes {
		t.Fatalf("default cloud disk reserve = %d, want %d", cfg.CloudWorkspaceDiskReserve, domain.CloudWorkspaceDiskReserveBytes)
	}
	if err := Load().Validate(); err != nil {
		t.Fatalf("authenticated cloud API behind a loopback TLS proxy was rejected: %v", err)
	}
	t.Setenv("O_CLOUD_TASK_DISK_RESERVE_BYTES", "1073741824")
	if cfg := Load(); cfg.CloudWorkspaceDiskReserve != 1<<30 || cfg.Validate() != nil {
		t.Fatalf("custom cloud disk reserve was rejected: %+v, err=%v", cfg, cfg.Validate())
	}
	t.Setenv("O_CLOUD_TASK_DISK_RESERVE_BYTES", "-1")
	if err := Load().Validate(); err == nil {
		t.Fatal("negative cloud disk reserve was accepted")
	}
	t.Setenv("O_CLOUD_TASK_DISK_RESERVE_BYTES", "1073741824")
	t.Setenv("O_CLOUD_TASK_WORKSPACE_QUOTA_BYTES", "-1")
	if err := Load().Validate(); err == nil {
		t.Fatal("negative cloud workspace hard quota was accepted")
	}
	t.Setenv("O_CLOUD_TASK_WORKSPACE_QUOTA_BYTES", "8589934592")
	t.Setenv("O_QUOTA_HELPER_SOCKET", "relative.sock")
	if err := Load().Validate(); err == nil {
		t.Fatal("relative privileged helper socket path was accepted")
	}
	t.Setenv("O_QUOTA_HELPER_SOCKET", "/run/o-agent/quota.sock")
	t.Setenv("O_FRONTEND_ORIGIN", "https://o.example.test/path")
	if err := Load().Validate(); err == nil {
		t.Fatal("cloud frontend URL containing a path was accepted as an origin")
	}
	t.Setenv("O_EXECUTION_ROLE", "local")
	t.Setenv("O_FRONTEND_ORIGIN", "https://o.example.test")
	t.Setenv("O_AUTH_BOOTSTRAP_TOKEN", "")
	t.Setenv("O_QUOTA_HELPER_SOCKET", "")
	t.Setenv("O_CLOUD_TASK_WORKSPACE_QUOTA_BYTES", "")
	if err := Load().Validate(); err == nil {
		t.Fatal("local-role API exposed through a public TLS proxy was allowed without authentication")
	}
}

func TestAgentTempDirConfiguration(t *testing.T) {
	for _, key := range []string{"O_AGENT_TEMP_DIR", "AXIOM_AGENT_TEMP_DIR"} {
		t.Setenv(key, "")
	}
	if got, want := Load().AgentTempDir, filepath.Join(os.TempDir(), "Axiom", "agent-runs"); got != want {
		t.Fatalf("default AgentTempDir = %q, want %q", got, want)
	}
	t.Setenv("AXIOM_AGENT_TEMP_DIR", filepath.Join(t.TempDir(), "legacy"))
	if got := Load().AgentTempDir; got != os.Getenv("AXIOM_AGENT_TEMP_DIR") {
		t.Fatalf("legacy AgentTempDir = %q, want %q", got, os.Getenv("AXIOM_AGENT_TEMP_DIR"))
	}
	t.Setenv("O_AGENT_TEMP_DIR", filepath.Join(t.TempDir(), "current"))
	if got := Load().AgentTempDir; got != os.Getenv("O_AGENT_TEMP_DIR") {
		t.Fatalf("AgentTempDir = %q, want %q", got, os.Getenv("O_AGENT_TEMP_DIR"))
	}
}

func TestNodeWorkerConfigurationMustBeCompleteAndLocal(t *testing.T) {
	for _, name := range []string{"O_NODE_CONTROL_URL", "AXIOM_NODE_CONTROL_URL", "O_NODE_CREDENTIAL", "AXIOM_NODE_CREDENTIAL", "O_NODE_PROVIDER_ID", "AXIOM_NODE_PROVIDER_ID", "O_NODE_WORKER_CONCURRENCY", "AXIOM_NODE_WORKER_CONCURRENCY", "O_EXECUTION_ROLE", "AXIOM_EXECUTION_ROLE"} {
		t.Setenv(name, "")
	}
	t.Setenv("O_NODE_CONTROL_URL", "https://o.example.test")
	if err := Load().Validate(); err == nil {
		t.Fatal("partial local node configuration was accepted")
	}
	t.Setenv("O_NODE_CREDENTIAL", "paired-node-credential-0123456789")
	t.Setenv("O_NODE_PROVIDER_ID", "provider-local")
	if err := Load().Validate(); err != nil {
		t.Fatalf("complete local worker configuration was rejected: %v", err)
	}
	t.Setenv("O_NODE_WORKER_CONCURRENCY", "2")
	if got := Load().NodeWorkerConcurrency; got != 2 {
		t.Fatalf("configured local worker concurrency = %d, want 2", got)
	}
	t.Setenv("O_NODE_WORKER_CONCURRENCY", "64")
	if cfg := Load(); cfg.NodeWorkerConcurrency != 64 || cfg.Validate() != nil {
		t.Fatalf("higher local worker ceiling was rejected: %+v, err=%v", cfg, cfg.Validate())
	}
	t.Setenv("O_NODE_WORKER_CONCURRENCY", "65")
	if err := Load().Validate(); err == nil {
		t.Fatal("local worker concurrency above the supported scheduler ceiling was accepted")
	}
	t.Setenv("O_EXECUTION_ROLE", "cloud")
	if err := Load().Validate(); err == nil {
		t.Fatal("cloud role accepted a second remote node credential")
	}
}
