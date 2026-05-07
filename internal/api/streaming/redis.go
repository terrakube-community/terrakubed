package streaming

import (
	"context"
	"encoding/binary"
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
// The Go executor uses JDK-serialized keys and field names (for Java API compat), so we
// try both the plain key and the JDK-serialized key, and decode field values accordingly.
func (r *LogStreamReader) readFromRedis(ctx context.Context, jobID string) ([]byte, error) {
	// Try plain key first (simple clients, future Go-native executors)
	msgs, err := r.redis.XRange(ctx, jobID, "-", "+").Result()
	if err != nil || len(msgs) == 0 {
		// Try JDK-serialized key (current Go executor compatibility path)
		jdkKey := jdkSerialize(jobID)
		msgs2, err2 := r.redis.XRange(ctx, jdkKey, "-", "+").Result()
		if err2 != nil {
			if err != nil {
				return nil, fmt.Errorf("XRange plain key: %v; JDK key: %v", err, err2)
			}
			return nil, fmt.Errorf("stream %s is empty or does not exist", jobID)
		}
		msgs = msgs2
	}

	if len(msgs) == 0 {
		return nil, fmt.Errorf("stream %s is empty", jobID)
	}

	var sb strings.Builder
	for _, msg := range msgs {
		// Field names are JDK-serialized — check both serialized and plain forms
		isDone := false
		for k := range msg.Values {
			plain := jdkDeserialize(k)
			if plain == "done" {
				isDone = true
				break
			}
		}
		if isDone {
			continue
		}

		for k, v := range msg.Values {
			fieldName := jdkDeserialize(k)
			if fieldName == "output" {
				raw := fmt.Sprintf("%v", v)
				sb.WriteString(jdkDeserialize(raw))
				sb.WriteByte('\n')
				break
			}
		}
	}
	return []byte(sb.String()), nil
}

// ──────────────────────────────────────────────────
// JDK serialization helpers (mirrors executor/logs/redis.go)
// ──────────────────────────────────────────────────

// JDKSerialize encodes a string using Java's JdkSerializationRedisSerializer format.
// Exported so the LogsHandler can write compatible keys/values when writing to Redis.
// Format: 0xAC 0xED 0x00 0x05 0x74 {2-byte-BE-len} {utf8-bytes}
func JDKSerialize(s string) string {
	return jdkSerialize(s)
}

// jdkSerialize encodes a string using Java's JdkSerializationRedisSerializer format.
// Format: 0xAC 0xED 0x00 0x05 0x74 {2-byte-BE-len} {utf8-bytes}
func jdkSerialize(s string) string {
	b := []byte(s)
	n := len(b)
	buf := make([]byte, 7+n)
	buf[0] = 0xAC
	buf[1] = 0xED
	buf[2] = 0x00
	buf[3] = 0x05
	buf[4] = 0x74
	buf[5] = byte(n >> 8)
	buf[6] = byte(n)
	copy(buf[7:], b)
	return string(buf)
}

// jdkDeserialize decodes a JDK-serialized string.
// Returns the input unchanged if it does not match the JDK magic bytes.
func jdkDeserialize(s string) string {
	b := []byte(s)
	// Minimum: 7 bytes header
	if len(b) < 7 {
		return s
	}
	// Check magic: AC ED 00 05 74
	if b[0] != 0xAC || b[1] != 0xED || b[2] != 0x00 || b[3] != 0x05 || b[4] != 0x74 {
		return s
	}
	strLen := int(binary.BigEndian.Uint16(b[5:7]))
	if len(b) < 7+strLen {
		return s
	}
	return string(b[7 : 7+strLen])
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
