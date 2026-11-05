package execengine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func managedRequest(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body any) (*http.Response, *ExecutionError) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, NewExecutionError("executor_unavailable", "encode managed Agent request: "+err.Error())
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, NewExecutionError("executor_unavailable", "build managed Agent request: "+err.Error())
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		if value != "" {
			req.Header.Set(key, value)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, NewExecutionError("execution_timeout", "managed Agent request exceeded its deadline")
		}
		return nil, NewExecutionError("executor_failed", "managed Agent request failed: "+err.Error())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		const errorBodyProbeBytes int64 = 4096
		observedBytes, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, errorBodyProbeBytes+1))
		bodyLength := "unknown"
		switch {
		case resp.ContentLength >= 0:
			bodyLength = fmt.Sprintf("%d", resp.ContentLength)
		case observedBytes > errorBodyProbeBytes:
			bodyLength = fmt.Sprintf(">%d", errorBodyProbeBytes)
		case readErr == nil:
			bodyLength = fmt.Sprintf("%d", observedBytes)
		}
		code := "executor_failed"
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			code = "executor_unavailable"
		}
		return nil, NewExecutionError(code, fmt.Sprintf("managed Agent API returned HTTP %d (response_body_bytes=%s)", resp.StatusCode, bodyLength))
	}
	return resp, nil
}

func managedJSON(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body any, maxBytes int) (map[string]any, *ExecutionError) {
	resp, execErr := managedRequest(ctx, client, method, url, headers, body)
	if execErr != nil {
		return nil, execErr
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if err != nil {
		return nil, NewExecutionError("executor_failed", "read managed Agent response: "+err.Error())
	}
	if len(data) > maxBytes {
		return nil, NewExecutionError("output_too_large", "managed Agent response exceeded the configured byte limit")
	}
	value, err := decodeJSONNumbers(data)
	if err != nil {
		return nil, NewExecutionError("invalid_executor_output", "managed Agent response was not valid JSON")
	}
	object, ok := asJSONObject(value)
	if !ok {
		return nil, NewExecutionError("invalid_executor_output", "managed Agent response must be a JSON object")
	}
	return object, nil
}

func readSSE(ctx context.Context, response *http.Response, maxBytes int, consume func(map[string]any) (bool, *ExecutionError)) *ExecutionError {
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	var dataLines []string
	total := 0
	for {
		line, readBytes, tooLarge, err := readBoundedSSELine(reader, maxBytes-total, maxBytes)
		if tooLarge {
			return NewExecutionError("output_too_large", "managed Agent event stream exceeded the configured byte limit")
		}
		total += readBytes
		trimmed := strings.TrimRight(string(line), "\r\n")
		if trimmed == "" {
			if len(dataLines) > 0 {
				data := strings.Join(dataLines, "\n")
				dataLines = dataLines[:0]
				if data != "[DONE]" {
					value, decodeErr := decodeJSONNumbers([]byte(data))
					if decodeErr != nil {
						return NewExecutionError("invalid_executor_output", "managed Agent emitted invalid SSE JSON")
					}
					if event, ok := asJSONObject(value); ok {
						done, execErr := consume(event)
						if execErr != nil || done {
							return execErr
						}
					}
				}
			}
		} else if payload, ok := strings.CutPrefix(trimmed, "data:"); ok {
			dataLines = append(dataLines, strings.TrimSpace(payload))
		}
		if err != nil {
			if ctx.Err() != nil {
				return NewExecutionError("execution_timeout", "managed Agent event stream exceeded its deadline")
			}
			if err == io.EOF {
				return NewExecutionError("executor_failed", "managed Agent stream closed before completion")
			}
			return NewExecutionError("executor_failed", "read managed Agent event stream: "+err.Error())
		}
	}
}

// readBoundedSSELine reads one physical SSE line without allowing bufio to
// accumulate an attacker-controlled, unterminated line. Both the remaining
// stream budget and the per-line budget include the trailing newline.
func readBoundedSSELine(reader *bufio.Reader, remainingBytes, maxLineBytes int) ([]byte, int, bool, error) {
	var line []byte
	readBytes := 0
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > remainingBytes-readBytes || len(fragment) > maxLineBytes-readBytes {
			return nil, readBytes + len(fragment), true, nil
		}
		readBytes += len(fragment)
		line = append(line, fragment...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, readBytes, false, err
	}
}

func managedCleanup(client *http.Client, headers map[string]string, cancelURL string, cancelBody any, deleteURL string, deleteSession bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if cancelURL != "" {
		if resp, _ := managedRequest(ctx, client, http.MethodPost, cancelURL, headers, cancelBody); resp != nil {
			resp.Body.Close()
		}
	}
	if deleteSession {
		if resp, _ := managedRequest(ctx, client, http.MethodDelete, deleteURL, headers, nil); resp != nil {
			resp.Body.Close()
		}
	}
}
