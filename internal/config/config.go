package config

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"

	"github.com/terrakube-community/terrakubed/internal/model"
)

type Config struct {
	// Mixed Config
	Port                      string
	AzBuilderRegistry         string
	AzBuilderApiUrl           string
	RegistryStorageType       string
	AwsBucketName             string
	AwsRegion                 string
	AwsAccessKey              string
	AwsSecretKey              string
	AwsEndpoint               string
	PatSecret                 string
	InternalSecret            string
	AzureStorageAccountName   string
	AzureStorageAccountKey    string
	AzureStorageContainerName string
	GcpStorageProjectId       string
	GcpStorageBucketName      string
	GcpStorageCredentials     string

	// Registry Auth
	AuthValidationType string // LOCAL or DEX
	IssuerUri          string
	AppClientId        string
	TerrakubeUiURL     string

	// Executor Specific
	Mode                    string
	EphemeralJobData        *model.TerraformJob
	TerrakubeRegistryDomain string
	StorageType             string

	// API Specific
	DatabaseURL   string
	Hostname      string
	ApiPort       string
	OwnerGroup    string
	RedisAddress  string
	RedisPassword string

	// API → Kubernetes executor config
	ExecutorNamespace      string
	ExecutorImage          string
	ExecutorSecretName     string
	ExecutorServiceAccount string
}

func getEnvWithFallback(primary, fallback string) string {
	val := os.Getenv(primary)
	if val == "" {
		return os.Getenv(fallback)
	}
	return val
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

// getEnvChain tries multiple environment variable names in order, returning the first non-empty value.
func getEnvChain(keys ...string) string {
	for _, key := range keys {
		if val := os.Getenv(key); val != "" {
			return val
		}
	}
	return ""
}

func getStorageType() string {
	st := os.Getenv("STORAGE_TYPE")
	if st != "" {
		return st
	}
	tst := os.Getenv("TerraformStateType")
	switch tst {
	case "AwsTerraformStateImpl":
		return "AWS"
	case "AzureTerraformStateImpl":
		return "AZURE"
	case "GcpTerraformStateImpl":
		return "GCP"
	case "LocalTerraformStateImpl", "":
		return "LOCAL"
	}
	return "LOCAL"
}

// getExecutorMode determines if executor runs in BATCH (ephemeral) or ONLINE mode.
// Java API sets ExecutorFlagBatch=true and EPHEMERAL_JOB_DATA on ephemeral K8s Jobs.
func getExecutorMode() string {
	if mode := os.Getenv("EXECUTOR_MODE"); mode != "" {
		return mode
	}
	// Java API sets EphemeralFlagBatch=true for ephemeral executors
	if os.Getenv("EphemeralFlagBatch") == "true" || os.Getenv("ExecutorFlagBatch") == "true" {
		return "BATCH"
	}
	// Auto-detect: if EphemeralJobData is present, run in batch mode
	if os.Getenv("EphemeralJobData") != "" || os.Getenv("EPHEMERAL_JOB_DATA") != "" {
		return "BATCH"
	}
	return ""
}

// getAwsAccessKey returns the AWS access key, or empty string if IAM role auth is enabled.
// When AwsEnableRoleAuth=true, we force empty credentials so IRSA/Pod Identity is used.
func getAwsAccessKey() string {
	if getEnv("AwsEnableRoleAuth", "false") == "true" {
		log.Println("AwsEnableRoleAuth=true, skipping static AWS credentials (using IAM role)")
		return ""
	}
	return getEnvChain("AwsStorageAccessKey", "AWS_ACCESS_KEY_ID")
}

// getAwsSecretKey returns the AWS secret key, or empty string if IAM role auth is enabled.
func getAwsSecretKey() string {
	if getEnv("AwsEnableRoleAuth", "false") == "true" {
		return ""
	}
	return getEnvChain("AwsStorageSecretKey", "AWS_SECRET_ACCESS_KEY")
}

// buildDatabaseURL constructs a PostgreSQL connection URL from Java-style env vars.
// Java API uses: DatasourceHostname, DatasourceDatabase, DatasourceUser, DatasourcePassword,
// DatasourcePort (default: 5432), DatasourceSslMode (default: disable).
// If DATABASE_URL is provided directly, it takes precedence.
func buildDatabaseURL() string {
	// Check if DATABASE_URL is provided directly
	if url := getEnvWithFallback("DATABASE_URL", "DatabaseUrl"); url != "" {
		return url
	}

	// Build from individual env vars (matching Java API's application.properties)
	host := getEnv("DatasourceHostname", "")
	if host == "" {
		return "" // No database configured
	}

	dbName := getEnv("DatasourceDatabase", "")
	user := getEnv("DatasourceUser", "")
	password := getEnv("DatasourcePassword", "")
	port := getEnv("DatasourcePort", "5432")
	sslMode := getEnv("DatasourceSslMode", "disable")

	// Use net/url to properly encode credentials (password may contain special chars)
	u := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     fmt.Sprintf("%s:%s", host, port),
		Path:     dbName,
		RawQuery: fmt.Sprintf("sslmode=%s", sslMode),
	}
	return u.String()
}

func buildRedisAddress() string {
	host := getEnvChain("TerrakubeRedisHostname", "REDIS_HOST")
	if host == "" {
		return ""
	}
	port := getEnvChain("TerrakubeRedisPort", "REDIS_PORT")
	if port == "" {
		port = "6379"
	}
	return host + ":" + port
}

func LoadConfig() (*Config, error) {
	cfg := &Config{
		// Registry
		Port:                getEnv("PORT", "8075"),
		AzBuilderRegistry:   getEnv("AzBuilderRegistry", "http://localhost:8075"),
		AzBuilderApiUrl:     getEnv("AzBuilderApiUrl", "http://localhost:8081"),
		RegistryStorageType: getEnv("RegistryStorageType", "AWS"),
		AwsBucketName:       getEnvChain("AwsStorageBucketName", "AWS_BUCKET_NAME", "AwsTerraformStateBucketName", "AwsTerraformOutputBucketName"),
		AwsRegion:           getEnvChain("AwsStorageRegion", "AWS_REGION", "AwsTerraformStateRegion", "AwsTerraformOutputRegion"),
		AwsAccessKey:        getAwsAccessKey(),
		AwsSecretKey:        getAwsSecretKey(),
		AwsEndpoint:         getEnv("AwsEndpoint", ""),

		PatSecret:                 getEnv("PatSecret", ""),
		InternalSecret:            getEnv("InternalSecret", ""),
		AzureStorageAccountName:   getEnv("AzureStorageAccountName", ""),
		AzureStorageAccountKey:    getEnv("AzureStorageAccountKey", ""),
		AzureStorageContainerName: getEnv("AzureStorageContainerName", ""),
		GcpStorageProjectId:       getEnv("GcpStorageProjectId", ""),
		GcpStorageBucketName:      getEnv("GcpStorageBucketName", ""),
		GcpStorageCredentials:     getEnv("GcpStorageCredentials", ""),

		// Registry Auth
		AuthValidationType: getEnvWithFallback("AuthenticationValidationTypeRegistry", "AUTH_VALIDATION_TYPE"),
		IssuerUri:          getEnvWithFallback("DexIssuerUri", "APP_ISSUER_URI"),
		AppClientId:        getEnv("AppClientId", ""),
		TerrakubeUiURL:     getEnvWithFallback("TerrakubeUiURL", "TERRAKUBE_UI_URL"),

		// Executor
		Mode:                    getExecutorMode(),
		TerrakubeRegistryDomain: getEnvWithFallback("TERRAKUBE_REGISTRY_DOMAIN", "TerrakubeRegistryDomain"),
		StorageType:             getStorageType(),

		// API
		DatabaseURL:   buildDatabaseURL(),
		Hostname:      getEnvWithFallback("TERRAKUBE_HOSTNAME", "TerrakubeHostname"),
		ApiPort:       getEnv("API_PORT", "8080"),
		// Java API equivalent: ${TERRAKUBE_OWNER:TerrakubeOwner}
		// Try TERRAKUBE_OWNER first, then TerrakubeOwner env var, then hardcoded default.
		// getEnvWithFallback treats the second arg as another env var name (not a value),
		// so we need an explicit hardcoded fallback to match Java API's behaviour.
		OwnerGroup: func() string {
			if v := getEnvWithFallback("TERRAKUBE_OWNER", "TerrakubeOwner"); v != "" {
				return v
			}
			return "TerrakubeOwner"
		}(),
		RedisAddress:  buildRedisAddress(),
		RedisPassword: getEnvChain("TerrakubeRedisPassword", "REDIS_PASSWORD"),

		// Kubernetes executor.
		// The Helm chart sets these as literal container env vars using Java's
		// exact Spring property names — io.terrakube.executor.ephemeral.namespace
		// reads ${ExecutorEphemeralNamespace:terrakube}, .image reads
		// ${ExecutorEphemeralImage:...}, .secret reads ${ExecutorEphemeralSecret:...}.
		// These must be the FIRST names checked: a previous fix here added a
		// hardcoded "terrakube" fallback for namespace that happened to match
		// the deployment's real value by coincidence — it was never actually
		// reading ExecutorEphemeralNamespace, so the equivalent gap for Image
		// (no safe hardcoded fallback exists — Java's default is its own old
		// executor image, wrong for terrakubed) surfaced as a hard failure:
		// "spec.template.spec.containers[0].image: Required value".
		ExecutorNamespace: func() string {
			if v := getEnvChain("ExecutorEphemeralNamespace", "EXECUTOR_NAMESPACE", "TerrakubeExecutorNamespace"); v != "" {
				return v
			}
			return "terrakube"
		}(),
		ExecutorImage:          getEnvChain("ExecutorEphemeralImage", "EXECUTOR_IMAGE", "TerrakubeExecutorImage", "AzBuilderRegistry"),
		ExecutorSecretName:     getEnvChain("ExecutorEphemeralSecret", "EXECUTOR_SECRET_NAME", "TerrakubeExecutorSecret"),
		ExecutorServiceAccount: getEnvChain("EXECUTOR_SERVICE_ACCOUNT", "TerrakubeExecutorServiceAccount"),
	}

	// Override API / Secret if provided by executor envs
	if api := getEnvWithFallback("TERRAKUBE_API_URL", "TerrakubeApiUrl"); api != "" {
		cfg.AzBuilderApiUrl = api
	}
	if secret := getEnvWithFallback("TERRAKUBE_INTERNAL_SECRET", "InternalSecret"); secret != "" {
		cfg.InternalSecret = secret
	}

	if cfg.Mode == "BATCH" {
		jobData := getEnvChain("EphemeralJobData", "EPHEMERAL_JOB_DATA")
		if jobData == "" {
			return nil, fmt.Errorf("BATCH mode but EphemeralJobData/EPHEMERAL_JOB_DATA is empty")
		}

		decodedData, err := base64.StdEncoding.DecodeString(jobData)
		if err != nil {
			return nil, fmt.Errorf("failed to decode EPHEMERAL_JOB_DATA: %v", err)
		}

		var job model.TerraformJob
		if err := json.Unmarshal(decodedData, &job); err != nil {
			return nil, fmt.Errorf("failed to unmarshal EPHEMERAL_JOB_DATA: %v", err)
		}
		cfg.EphemeralJobData = &job
	}

	return cfg, nil
}
