package convert

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// SSEFrame 表示一个完整的 SSE frame。
type SSEFrame struct {
	Event string
	Data  string
}

// ReadSSE 按 frame 读取 SSE，允许 data 跨多行，并保留 event 名称。
func ReadSSE(reader io.Reader, handle func(SSEFrame) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 16*1024*1024)
	var event string
	var data []string
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		frame := SSEFrame{Event: event, Data: strings.Join(data, "\n")}
		event = ""
		data = nil
		return handle(frame)
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch key {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("读取 SSE: %w", err)
	}
	return flush()
}

// WriteSSE 写出一个 SSE frame。
func WriteSSE(writer io.Writer, frame SSEFrame) error {
	if frame.Event != "" {
		if _, err := fmt.Fprintf(writer, "event: %s\n", frame.Event); err != nil {
			return err
		}
	}
	for _, line := range strings.Split(frame.Data, "\n") {
		if _, err := fmt.Fprintf(writer, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := io.WriteString(writer, "\n")
	return err
}
