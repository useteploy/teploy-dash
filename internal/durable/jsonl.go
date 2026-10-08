package durable

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// ReadJSONL repairs only an unterminated final frame. Interior corruption
// fails closed. The repaired prefix commits durably before callers append.
func ReadJSONL(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	offset := 0
	for offset < len(data) {
		end := bytes.IndexByte(data[offset:], '\n')
		final := end < 0
		if final {
			end = len(data) - offset
		}
		line := bytes.TrimSpace(data[offset : offset+end])
		if len(line) > 0 && !json.Valid(line) {
			if !final {
				return nil, fmt.Errorf("corrupt JSONL frame at byte %d", offset)
			}
			data = data[:offset]
			if err := Replace(path, data, 0600); err != nil {
				return nil, err
			}
			return data, nil
		}
		if final {
			data = append(data, '\n')
			if err := Replace(path, data, 0600); err != nil {
				return nil, err
			}
			return data, nil
		}
		offset += end + 1
	}
	return data, nil
}
