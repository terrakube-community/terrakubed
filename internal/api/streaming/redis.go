package streaming

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/terrakube-community/terrakubed/internal/storage"
)

// LogStreamReader reads job logs from Redis Streams with fallback to object storage.
// Mirrors the Java StreamingServiceRedis + TerraformOutputController pattern.
type LogStreamReader struct {
	redis   *redis.Client
	storage storage.StorageService
}

// NewLogStreamReader creates a LogStreamReader.
// redisClient may be nil — in that case Redis lookups are skipped and only storage is used.
func NewLogStreamReader(redisClient *redis.Client, storageService storage.StorageService) *LogStreamReader {
	return &LogStreamReader{
		redis:   redisClient,
		storage: storageService,
	}
}

// GetStepOutput returns the log output for a step.
// Strategy (mirrors Java TerraformOutputController.getFile()):
//  1. Try Redis stream first (job is still running or TTL hasn't expired)
//  2. Fall back to object storage (job finished, logs uploaded to S3/Azure/GCP)
func (r *LogStreamReader) GetStepOutput(ctx context.Context, orgID, jobID, stepID string) ([]byte, error) {
	// 1. Try Redis — stream key is the jobId (matches executor RedisStreamer)
	if r.redis != nil {
		data, err := r.readFromRedis(ctx, jobID)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			log.Printf("Serving live logs from Redis stream (jobId=%s, stepId=%s, bytes=%d)", jobID, stepID, len(data))
			return data, nil
		}
		if err != nil {
			log.Printf("Redis read failed for jobId=%s: %v (falling back to storage)", jobID, err)
		}
	}

	// 2. Fall back to object storage
	return r.readFromStorage(orgID, jobID, stepID)
}

// readFromRedis reads all entries from a Redis Stream and returns concatenated log output.
// Matches the Java LogsConsumer / StreamingService.getCurrentLogs() pattern.
func (r *LogStreamReader) readFromRedis(ctx context.Context, jobID string) ([]byte, error) {
	msgs, err := r.redis.XRange(ctx, jobID, "-", "+").Result()
	if err != nil {
		return nil, fmt.Errorf("XRange %s: %w", jobID, err)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("stream %s is empty or does not exist", jobID)
	}

	var sb strings.Builder
	for _, msg := range msgs {
		// Skip sentinel done messages
		if _, done := msg.Values["done"]; done {
			continue
		}
		if out, ok := msg.Values["output"]; ok {
			sb.WriteString(fmt.Sprintf("%v", out))
			sb.WriteByte('\n')
		}
	}
	return []byte(sb.String()), nil
}

// readFromStorage downloads the log file from object storage.
// Path matches executor status.saveOutput(): tfoutput/{orgId}/{jobId}/{stepId}.tfoutput
func (r *LogStreamReader) readFromStorage(orgID, jobID, stepID string) ([]byte, error) {
	if r.storage == nil {
		return nil, fmt.Errorf("no storage configured")
	}

	path := fmt.Sprintf("tfoutput/%s/%s/%s.tfoutput", orgID, jobID, stepID)
	log.Printf("Serving logs from storage: %s", path)

	rc, err := r.storage.DownloadFile(path)
	if err != nil {
		return nil, fmt.Errorf("storage download %s: %w", path, err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("reading storage response: %w", err)
	}
	return data, nil
}
