package main

import (
	"log"
	"os"
	"strconv"
	"sync"

	api "github.com/terrakube-community/terrakubed/internal/api"
	"github.com/terrakube-community/terrakubed/internal/config"
	"github.com/terrakube-community/terrakubed/internal/executor"
	"github.com/terrakube-community/terrakubed/internal/executor/terraform"
	"github.com/terrakube-community/terrakubed/internal/registry"
)

func main() {
	serviceType := os.Getenv("SERVICE_TYPE")
	if serviceType == "" {
		// Default to running all services for easy local development
		serviceType = "all"
	}

	// Automatically set PORT based on SERVICE_TYPE if it is not provided
	if os.Getenv("PORT") == "" {
		switch serviceType {
		case "api":
			os.Setenv("PORT", "8080")
		case "executor":
			os.Setenv("PORT", "8090")
		case "registry":
			os.Setenv("PORT", "8075")
		}
	}
	if os.Getenv("API_PORT") == "" {
		os.Setenv("API_PORT", "8080")
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	log.Printf("Starting Terrakubed (Service Type: %s)\n", serviceType)

	var wg sync.WaitGroup

	switch serviceType {
	case "api":
		wg.Add(1)
		go func() {
			defer wg.Done()
			startAPI(cfg)
		}()
	case "registry":
		wg.Add(1)
		go func() {
			defer wg.Done()
			startRegistry(cfg)
		}()
	case "executor":
		wg.Add(1)
		go func() {
			defer wg.Done()
			startExecutor(cfg)
		}()
	case "install-terraform":
		installTerraform(cfg)
	case "all":
		wg.Add(3)
		go func() {
			defer wg.Done()
			startAPI(cfg)
		}()
		go func() {
			defer wg.Done()
			startRegistry(cfg)
		}()
		go func() {
			defer wg.Done()
			startExecutor(cfg)
		}()
	default:
		log.Fatalf("Unknown SERVICE_TYPE: %s. Supported values are: api, registry, executor, install-terraform, all", serviceType)
	}

	wg.Wait()
}

// installTerraform downloads (or confirms the cached presence of) the
// Terraform/OpenTofu version requested by the job's EphemeralJobData, then
// exits. Used as the executor Job's init container so the download happens
// on a volume shared with, but not part of, the executor container's own
// writable layer — the executor container then just execs the already
// in-place binary instead of dropping and running a freshly fetched one
// itself, which is what a "new binary written and executed" runtime
// detection (e.g. Falco) would otherwise flag.
func installTerraform(cfg *config.Config) {
	if cfg.EphemeralJobData == nil {
		log.Fatal("install-terraform requires EphemeralJobData/EPHEMERAL_JOB_DATA")
	}

	vm := terraform.NewVersionManager()
	execPath, err := vm.Install(cfg.EphemeralJobData.TerraformVersion, cfg.EphemeralJobData.Tofu)
	if err != nil {
		log.Fatalf("Failed to install terraform %s: %v", cfg.EphemeralJobData.TerraformVersion, err)
	}

	log.Printf("Terraform ready at: %s", execPath)
}

func startAPI(cfg *config.Config) {
	log.Println("API service is starting...")

	port, _ := strconv.Atoi(cfg.ApiPort)

	apiConfig := api.Config{
		DatabaseURL:    cfg.DatabaseURL,
		Port:           port,
		Hostname:       cfg.Hostname,
		DexIssuerURI:   cfg.IssuerUri,
		PatSecret:      cfg.PatSecret,
		InternalSecret: cfg.InternalSecret,
		OwnerGroup:     cfg.OwnerGroup,
		UIURL:          cfg.TerrakubeUiURL,
		StorageType:    cfg.StorageType,
		RedisAddress:   cfg.RedisAddress,
		RedisPassword:  cfg.RedisPassword,

		// Kubernetes executor (used by Go API job scheduler)
		ExecutorNamespace:      cfg.ExecutorNamespace,
		ExecutorImage:          cfg.ExecutorImage,
		ExecutorSecretName:     cfg.ExecutorSecretName,
		ExecutorServiceAccount: cfg.ExecutorServiceAccount,
	}

	server, err := api.NewServer(apiConfig)
	if err != nil {
		log.Fatalf("Failed to start API server: %v", err)
	}
	defer server.Close()

	if err := server.Start(); err != nil {
		log.Fatalf("API server failed: %v", err)
	}
}

func startRegistry(cfg *config.Config) {
	log.Println("Registry service is starting...")
	registry.Start(cfg)
}

func startExecutor(cfg *config.Config) {
	log.Println("Executor service is starting...")
	executor.Start(cfg)
}
