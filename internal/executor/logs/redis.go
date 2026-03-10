package logs

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// jdkSerialize produces the Java JdkSerializationRedisSerializer byte representation of a
// java.lang.String. Spring Data RedisTemplate uses this serializer by default (when no explicit
// serializer is configured), so both the stream key and every field name/value in the stream
// record must use this format to be readable by the Java API's StreamingService.
//
// Format: STREAM_MAGIC(AC ED) + STREAM_VERSION(00 05) + TC_STRING(74) + 2-byte-BE-len + UTF-8
func jdkSerialize(s string) string {
	b := []byte(s)
	n := len(b)
	buf := make([]byte, 7+n)
	buf[0] = 0xAC
	buf[1] = 0xED
	buf[2] = 0x00
	buf[3] = 0x05
	buf[4] = 0x74 // TC_STRING
	buf[5] = byte(n >> 8)
	buf[6] = byte(n)
	copy(buf[7:], b)
	return string(buf)
}

// RedisStreamer writes log lines to a Redis Stream so the API can serve them
// in real-time via the /tfoutput/v1/... endpoint.
// Matches the Java LogsServiceRedis + LogsConsumer pattern.
type RedisStreamer struct {
	client     *redis.Client
	jobId      string // raw job ID (used for logging only)
	jdkJobId   string // JDK-serialized job ID (used as Redis stream key)
	jdkStepId  string // JDK-serialized step ID
	lineNumber atomic.Int32
	buf        strings.Builder
}

func NewRedisStreamer(addr, password, jobId, stepId string) (*RedisStreamer, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       0,
	})

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis at %s: %w", addr, err)
	}

	rs := &RedisStreamer{
		client:    rdb,
		jobId:     jobId,
		jdkJobId:  jdkSerialize(jobId),
		jdkStepId: jdkSerialize(stepId),
	}

	// Setup consumer groups (matching Java LogsServiceRedis.setupConsumerGroups).
	// Group names are NOT serialized — Spring passes them raw to the Redis command.
	ctx := context.Background()
	_ = rdb.XGroupCreateMkStream(ctx, rs.jdkJobId, "CLI", "0").Err()
	_ = rdb.XGroupCreateMkStream(ctx, rs.jdkJobId, "UI", "0").Err()

	return rs, nil
}

func (r *RedisStreamer) Write(p []byte) (n int, err error) {
	os.Stdout.Write(p)

	r.buf.WriteString(string(p))

	for {
		content := r.buf.String()
		idx := strings.IndexByte(content, '\n')
		if idx == -1 {
			break
		}
		line := content[:idx]
		remaining := content[idx+1:]
		r.buf.Reset()
		r.buf.WriteString(remaining)

		lineNum := r.lineNumber.Add(1)

		// XAdd to Redis Stream with all keys/values JDK-serialized to match
		// the Java RedisTemplate's default JdkSerializationRedisSerializer.
		err := r.client.XAdd(context.Background(), &redis.XAddArgs{
			Stream: r.jdkJobId,
			Values: map[string]interface{}{
				jdkSerialize("jobId"):      jdkSerialize(r.jobId),
				jdkSerialize("stepId"):     r.jdkStepId,
				jdkSerialize("lineNumber"): jdkSerialize(fmt.Sprintf("%d", lineNum)),
				jdkSerialize("output"):     jdkSerialize(line),
			},
		}).Err()
		if err != nil {
			log.Printf("Warning: failed to send log line to Redis: %v", err)
		} else if lineNum == 1 {
			log.Printf("First log line sent to Redis stream (jobId=%s)", r.jobId)
		}
	}

	return len(p), nil
}

func (r *RedisStreamer) Close() error {
	ctx := context.Background()

	// Flush any remaining content in buffer
	if r.buf.Len() > 0 {
		lineNum := r.lineNumber.Add(1)
		_ = r.client.XAdd(ctx, &redis.XAddArgs{
			Stream: r.jdkJobId,
			Values: map[string]interface{}{
				jdkSerialize("jobId"):      jdkSerialize(r.jobId),
				jdkSerialize("stepId"):     r.jdkStepId,
				jdkSerialize("lineNumber"): jdkSerialize(fmt.Sprintf("%d", lineNum)),
				jdkSerialize("output"):     jdkSerialize(r.buf.String()),
			},
		}).Err()
		r.buf.Reset()
	}

	// Sentinel so consumers know the stream is complete.
	_ = r.client.XAdd(ctx, &redis.XAddArgs{
		Stream: r.jdkJobId,
		Values: map[string]interface{}{
			jdkSerialize("jobId"):  jdkSerialize(r.jobId),
			jdkSerialize("stepId"): r.jdkStepId,
			jdkSerialize("done"):   jdkSerialize("true"),
		},
	}).Err()

	// Set TTL so the UI has time to read remaining logs after job completes.
	r.client.Expire(ctx, r.jdkJobId, 5*time.Minute)

	return r.client.Close()
}
