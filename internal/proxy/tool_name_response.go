package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

func restoreNonStreamingToolNames(body []byte, names toolNameMapping) ([]byte, int, error) {
	if len(names) == 0 {
		return body, 0, nil
	}
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, 0, fmt.Errorf("decode upstream response body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, 0, fmt.Errorf("decode upstream response body: trailing JSON value")
	}
	if root == nil {
		return nil, 0, fmt.Errorf("upstream response body must be a JSON object")
	}

	changed := 0
	if content, ok := root["content"].([]any); ok {
		for _, raw := range content {
			block, ok := raw.(map[string]any)
			if !ok || block["type"] != "tool_use" {
				continue
			}
			if restoreToolName(block, names) {
				changed++
			}
		}
	}
	if changed == 0 {
		return body, 0, nil
	}
	transformed, err := json.Marshal(root)
	if err != nil {
		return nil, 0, fmt.Errorf("encode tool-restored upstream response: %w", err)
	}
	return transformed, changed, nil
}

func copySSEWithRestoredToolNames(destination io.Writer, source io.Reader, names toolNameMapping) (int64, int, error) {
	reader := bufio.NewReader(source)
	var event []byte
	var written int64
	changed := 0
	flushEvent := func() error {
		if len(event) == 0 {
			return nil
		}
		transformed, eventChanges, err := restoreSSEEventToolName(event, names)
		if err != nil {
			return err
		}
		n, err := writeAll(destination, transformed)
		written += int64(n)
		changed += eventChanges
		event = event[:0]
		return err
	}

	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			event = append(event, line...)
			trimmed := bytes.TrimSuffix(line, []byte{'\n'})
			trimmed = bytes.TrimSuffix(trimmed, []byte{'\r'})
			if len(trimmed) == 0 {
				if err := flushEvent(); err != nil {
					return written, changed, err
				}
			}
		}
		if readErr == nil {
			continue
		}
		if readErr != io.EOF {
			return written, changed, readErr
		}
		if err := flushEvent(); err != nil {
			return written, changed, err
		}
		return written, changed, nil
	}
}

func restoreSSEEventToolName(event []byte, names toolNameMapping) ([]byte, int, error) {
	if len(names) == 0 || !bytes.Contains(event, []byte("content_block_start")) {
		return event, 0, nil
	}
	lines := bytes.SplitAfter(event, []byte{'\n'})
	dataLines := make([]int, 0, 1)
	var payload []byte
	for index, rawLine := range lines {
		line := bytes.TrimSuffix(rawLine, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := line[len("data:"):]
		data = bytes.TrimPrefix(data, []byte{' '})
		if len(payload) > 0 {
			payload = append(payload, '\n')
		}
		payload = append(payload, data...)
		dataLines = append(dataLines, index)
	}
	if len(dataLines) == 0 {
		return event, 0, nil
	}

	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, 0, fmt.Errorf("decode upstream content_block_start event: %w", err)
	}
	if envelope["type"] != "content_block_start" {
		return event, 0, nil
	}
	block, ok := envelope["content_block"].(map[string]any)
	if !ok || block["type"] != "tool_use" || !restoreToolName(block, names) {
		return event, 0, nil
	}
	transformed, err := json.Marshal(envelope)
	if err != nil {
		return nil, 0, fmt.Errorf("encode tool-restored content_block_start event: %w", err)
	}

	var output bytes.Buffer
	firstDataLine := dataLines[0]
	dataLineSet := make(map[int]struct{}, len(dataLines))
	for _, index := range dataLines {
		dataLineSet[index] = struct{}{}
	}
	for index, line := range lines {
		if _, isData := dataLineSet[index]; !isData {
			output.Write(line)
			continue
		}
		if index != firstDataLine {
			continue
		}
		output.WriteString("data: ")
		output.Write(transformed)
		switch {
		case bytes.HasSuffix(line, []byte("\r\n")):
			output.WriteString("\r\n")
		case bytes.HasSuffix(line, []byte{'\n'}):
			output.WriteByte('\n')
		}
	}
	return output.Bytes(), 1, nil
}

func restoreToolName(value map[string]any, names toolNameMapping) bool {
	name, ok := value["name"].(string)
	if !ok {
		return false
	}
	original, ok := names[name]
	if !ok {
		return false
	}
	value["name"] = original
	return true
}

func writeAll(destination io.Writer, data []byte) (int, error) {
	written := 0
	for written < len(data) {
		n, err := destination.Write(data[written:])
		written += n
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}
