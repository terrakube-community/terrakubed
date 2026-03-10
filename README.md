# Terrakubed

**The unified, high-performance Go backend for [Terrakube](https://github.com/AzBuilder/terrakube).**

> A single lightweight binary that replaces the Java executor and registry microservices — fully compatible with the Terrakube Java API.

[![Release](https://img.shields.io/github/v/release/terrakube-community/terrakubed)](https://github.com/terrakube-community/terrakubed/releases)
[![Docker Pulls](https://img.shields.io/docker/pulls/terrakubecommunity/terrakubed)](https://hub.docker.com/r/terrakubecommunity/terrakubed)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

---

## What is Terrakubed?

Terrakubed is a Go reimplementation of the Terrakube executor and registry services. It is a **drop-in replacement** for the Java-based `executor` and `registry` containers — no changes to your Terrakube API, UI, or Kubernetes configuration are required.

**Why Go?**

| | Java executor | Terrakubed |
|---|---|---|
| Startup time | ~15–30 s (JVM warmup) | < 1 s |
| Container image size | ~500 MB | ~25 MB |
| Memory (idle) | ~256–512 MB | ~15 MB |
| Dynamic TF version install | ❌ (fixed image) | ✅ (downloads on demand) |
| Terraform CLI remote backend | ✅ | ✅ |

---

## Features

- **Drop-in replacement** — uses the same Terrakube Java API endpoints and environment variables as the official executor/registry containers.
- **Dynamic Terraform / OpenTofu versioning** — downloads and caches the exact binary version your workspace requires at runtime.
- **Full job lifecycle** — `terraformPlan`, `terraformPlanDestroy`, `terraformApply`, `terraformDestroy`, `customScripts`, and approval gates.
- **Terraform CLI remote backend** — run `terraform plan` / `apply` from your local machine using a Terrakube workspace as the remote backend.
- **Private module & provider registry** — fully compliant with the Terraform registry protocol (`/terraform/modules/v1/` and `/terraform/providers/v1/`).
- **Multi-cloud state storage** — native SDKs for AWS S3, Azure Blob Storage, and Google Cloud Storage. State is managed directly by the backend (no manual download/upload).
- **Built-in API service** — optional Go replacement for the Terrakube Java API (PostgreSQL + JSON:API + GraphQL).
- **Built-in Slack notifications** — zero-YAML alerts for plan pending, no changes, approved, success, and failure.
- **Live log streaming** — real-time executor logs via Redis pub/sub.
- **Multi-architecture Docker images** — `linux/amd64` and `linux/arm64`.

---

## Architecture

Terrakubed compiles into a **single binary** that activates services based on the `SERVICE_TYPE` environment variable:

```
SERVICE_TYPE=executor   →  Job runner (ephemeral K8s jobs or polling mode)
SERVICE_TYPE=registry   →  Terraform module & provider registry
SERVICE_TYPE=api        →  REST/GraphQL API (Go replacement for the Java API)
SERVICE_TYPE=all        →  All three services in one process (local dev)
```

Default ports:

| Service | Port |
|---|---|
| API | 8080 |
| Registry | 8075 |
| Executor | 8090 |

---

## Getting Started

### Kubernetes (recommended — drop-in replacement)

The most common deployment is to replace only the executor container. Change the image in your executor `Deployment` or Job template:

```yaml
# In your Terrakube executor Deployment / Job template
image: terrakubecommunity/terrakubed:v0.1.0
```

All existing environment variables (storage keys, API URL, secrets) are accepted unchanged.

### Docker Compose (local development)

```bash
git clone https://github.com/terrakube-community/terrakubed.git
cd terrakubed

# Run all services (API + Registry + Executor) in one container
docker run --rm \
  -e SERVICE_TYPE=all \
  -e STORAGE_TYPE=LOCAL \
  -p 8080:8080 \
  -p 8075:8075 \
  -p 8090:8090 \
  terrakubecommunity/terrakubed:v0.1.0
```

### Build from Source

```bash
git clone https://github.com/terrakube-community/terrakubed.git
cd terrakubed

go mod download
go build -o terrakubed cmd/terrakubed/main.go

SERVICE_TYPE=all ./terrakubed
```

---

## Configuration

Terrakubed accepts the same environment variables as the official Terrakube Java containers.

### Core

| Variable | Description | Default |
|---|---|---|
| `SERVICE_TYPE` | `executor` \| `registry` \| `api` \| `all` | `executor` |
| `STORAGE_TYPE` | `AWS` \| `AZURE` \| `GCP` \| `LOCAL` | `LOCAL` |
| `AzBuilderApiUrl` / `TERRAKUBE_API_URL` | Terrakube Java API base URL | `http://localhost:8081` |
| `InternalSecret` / `TERRAKUBE_INTERNAL_SECRET` | Shared secret for internal JWT tokens | — |
| `TerrakubeUiURL` / `TERRAKUBE_UI_URL` | UI base URL (used in Slack deep links) | — |

### Storage — AWS S3

| Variable | Description |
|---|---|
| `AwsStorageBucketName` / `AWS_BUCKET_NAME` | S3 bucket name |
| `AwsStorageRegion` / `AWS_REGION` | AWS region |
| `AwsStorageAccessKey` / `AWS_ACCESS_KEY_ID` | Access key (omit for IRSA / Pod Identity) |
| `AwsStorageSecretKey` / `AWS_SECRET_ACCESS_KEY` | Secret key (omit for IRSA / Pod Identity) |
| `AwsEndpoint` | Custom S3 endpoint (MinIO, Localstack, etc.) |
| `AwsEnableRoleAuth` | Set `true` to force IAM role auth (skip static keys) |

### Storage — Azure Blob

| Variable | Description |
|---|---|
| `AzureStorageAccountName` | Storage account name |
| `AzureStorageContainerName` | Container name |
| `AzureStorageAccountKey` | Storage account key |

### Storage — Google Cloud Storage

| Variable | Description |
|---|---|
| `GcpStorageBucketName` | GCS bucket name |
| `GcpStorageProjectId` | GCP project ID |
| `GcpStorageCredentials` | Service account JSON (base64-encoded) |

### Registry

| Variable | Description | Default |
|---|---|---|
| `AzBuilderRegistry` / `TERRAKUBE_REGISTRY_DOMAIN` | Registry base URL | `http://localhost:8075` |
| `AuthenticationValidationTypeRegistry` | `LOCAL` or `DEX` | `LOCAL` |
| `DexIssuerUri` / `APP_ISSUER_URI` | OIDC issuer URI (required when using DEX) | — |

### API Service (Go API)

| Variable | Description |
|---|---|
| `DATABASE_URL` / `DatasourceHostname` | PostgreSQL connection URL or hostname |
| `DatasourceDatabase` | Database name |
| `DatasourceUser` | Database user |
| `DatasourcePassword` | Database password |
| `DatasourcePort` | Database port (default: `5432`) |
| `TerrakubeHostname` / `TERRAKUBE_HOSTNAME` | Public hostname for state/TFE URLs |
| `TerrakubeOwner` / `TERRAKUBE_OWNER` | Owner group name |
| `PatSecret` | Secret for PAT token signing |
| `TerrakubeRedisHostname` / `REDIS_HOST` | Redis hostname for live log streaming |
| `TerrakubeRedisPort` / `REDIS_PORT` | Redis port (default: `6379`) |
| `TerrakubeRedisPassword` / `REDIS_PASSWORD` | Redis password |

### Executor — Ephemeral (Kubernetes Jobs)

The Terrakube Java API creates a K8s Job for each run and passes the job data via environment variable:

| Variable | Description |
|---|---|
| `EphemeralFlagBatch` / `ExecutorFlagBatch` | Set `true` by the Java API to activate batch (ephemeral) mode |
| `EphemeralJobData` / `EPHEMERAL_JOB_DATA` | Base64-encoded JSON job payload (set by Java API) |

---

## Workflow Templates

### Plan Only

```yaml
flow:
  - type: "terraformPlan"
    name: "Plan"
    step: 100
```

### Plan + Apply with Approval Gate

```yaml
flow:
  - type: "terraformPlan"
    name: "Plan"
    step: 100
  - type: "terraformApply"
    name: "Apply"
    step: 200
    approval: true
```

### Destroy with Approval Gate

```yaml
flow:
  - type: "terraformPlanDestroy"
    name: "Plan Destroy"
    step: 100
  - type: "terraformDestroy"
    name: "Destroy"
    step: 200
    approval: true
```

### With Before / After Scripts

```yaml
flow:
  - type: "terraformPlan"
    name: "Plan"
    step: 100
    commands:
      - runtime: "BASH"
        priority: 10
        before: true
        script: |
          echo "Running before init"
      - runtime: "BASH"
        priority: 10
        after: true
        script: |
          echo "Plan complete"
          terraform version
          jq --version
```

---

## Terraform CLI Remote Backend

Terrakubed supports running `terraform plan` and `terraform apply` from your **local machine** using a Terrakube workspace as the remote backend — the same workflow as Terraform Cloud's remote operations.

### Configure your workspace

Add this to any `.tf` file in your project (or `backend.tf`):

```hcl
terraform {
  backend "remote" {
    hostname     = "terrakube-api.example.com"
    organization = "my-org"

    workspaces {
      name = "my-workspace"
    }
  }
}
```

### Authenticate

```bash
terraform login terrakube-api.example.com
```

### Run

```bash
terraform init
terraform plan    # executes in Terrakube, streams logs to your terminal
terraform apply
```

The Terraform CLI uploads your local configuration to Terrakube, which runs the job in Kubernetes and streams the output back to your terminal in real time.

---

## Slack Notifications

Terrakubed has built-in Slack notifications that require **no changes to your workflow YAML**. Configure them via workspace environment variables:

| Variable | Required | Description |
|---|---|---|
| `SLACK_WEBHOOK_URL` | Yes | Incoming webhook URL for your Slack channel |
| `ENABLE_SLACK_NOTIFICATIONS` | No | Set to `true` to enable full lifecycle notifications |

### Notification events

| Event | Trigger |
|---|---|
| ⏳ Plan Ready — Awaiting Approval | `ENABLE_SLACK_NOTIFICATIONS=true` + plan has changes (exit 2) |
| 💤 No Changes Detected | `ENABLE_SLACK_NOTIFICATIONS=true` + plan has no changes (exit 0) |
| ✅ Approved — Applying / Destroying | `ENABLE_SLACK_NOTIFICATIONS=true` + apply/destroy starts |
| 🚀 Apply / Destroy Completed | `ENABLE_SLACK_NOTIFICATIONS=true` + apply/destroy succeeds |
| 🔴 Failure | `SLACK_WEBHOOK_URL` set (regardless of `ENABLE_SLACK_NOTIFICATIONS`) |

> **Tip:** Set `SLACK_WEBHOOK_URL` globally so failures are always reported, and enable `ENABLE_SLACK_NOTIFICATIONS=true` per workspace for full lifecycle visibility.

Each notification includes the workspace name (with a deep link to the UI if `TerrakubeUiURL` is set), the repository source, branch, and Terraform/OpenTofu version.

---

## Kubernetes RBAC

If you run Terrakubed as an ephemeral executor (K8s Job per run), apply the bundled RBAC manifests so the executor pod can read its own job data:

```bash
kubectl apply -f https://raw.githubusercontent.com/terrakube-community/terrakubed/main/ephemeral-executor-config/service_account.yml
kubectl apply -f https://raw.githubusercontent.com/terrakube-community/terrakubed/main/ephemeral-executor-config/rbac_role.yml
kubectl apply -f https://raw.githubusercontent.com/terrakube-community/terrakubed/main/ephemeral-executor-config/rbac_role_binding.yml
```

These files are adapted from the upstream Terrakube project and are kept in sync.

---

## Compatibility

Terrakubed is tested against the **Terrakube Java API v2.27+**. It uses the same:

- REST endpoints (`/api/v1/`, `/logs/`, `/tfoutput/v1/`, `/context/v1/`)
- State storage paths (`tfstate/{orgId}/{wsId}/terraform.tfstate`)
- Plan file paths (`organization/{orgId}/workspace/{wsId}/job/{jobId}/plan/terraformLibrary.tfplan`)
- Job status values (`running`, `pending`, `completed`, `failed`, `queue`)
- Ephemeral job payload format (`EphemeralJobData` base64 JSON)

---

## Contributing

Contributions are welcome! Please open an issue or pull request on [GitHub](https://github.com/terrakube-community/terrakubed).

Areas where help is appreciated:

- Additional storage backends
- OpenTofu registry protocol enhancements
- Improved test coverage
- Documentation and examples

---

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.
