package accounting

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

const nonStreamingTailLimit = 2 << 20

type Usage struct {
	InputTokens           int64
	OutputTokens          int64
	CacheCreation5mTokens int64
	CacheCreation1hTokens int64
	CacheReadTokens       int64
	Seen                  bool
	Complete              bool
}

type Observer struct {
	upstream io.Reader
	sse      bool
	stream   sseObserver
	tail     []byte
}

func NewObserver(upstream io.Reader, contentType string) *Observer {
	return &Observer{upstream: upstream, sse: strings.Contains(strings.ToLower(contentType), "text/event-stream")}
}

func (o *Observer) Read(buffer []byte) (int, error) {
	n, err := o.upstream.Read(buffer)
	if n > 0 {
		if o.sse {
			o.stream.write(buffer[:n])
		} else {
			o.appendTail(buffer[:n])
		}
	}
	return n, err
}

func (o *Observer) Result(copyErr error, fallbackModel string) (Usage, string) {
	if o.sse {
		o.stream.finish()
		usage := o.stream.usage
		usage.Complete = copyErr == nil && o.stream.stopped
		model := o.stream.model
		if model == "" {
			model = fallbackModel
		}
		return usage, model
	}
	usage, model, ok := parseNonStreaming(o.tail)
	usage.Complete = copyErr == nil && ok
	if model == "" {
		model = fallbackModel
	}
	return usage, model
}

func (o *Observer) appendTail(data []byte) {
	if len(data) >= nonStreamingTailLimit {
		o.tail = append(o.tail[:0], data[len(data)-nonStreamingTailLimit:]...)
		return
	}
	if overflow := len(o.tail) + len(data) - nonStreamingTailLimit; overflow > 0 {
		copy(o.tail, o.tail[overflow:])
		o.tail = o.tail[:len(o.tail)-overflow]
	}
	o.tail = append(o.tail, data...)
}

type wireUsage struct {
	InputTokens        *int64 `json:"input_tokens"`
	OutputTokens       *int64 `json:"output_tokens"`
	CacheCreationInput *int64 `json:"cache_creation_input_tokens"`
	CacheReadInput     *int64 `json:"cache_read_input_tokens"`
	CacheCreation      *struct {
		Ephemeral5mInput *int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1hInput *int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func (u *Usage) apply(raw wireUsage) {
	if raw.InputTokens != nil {
		u.InputTokens = *raw.InputTokens
		u.Seen = true
	}
	if raw.OutputTokens != nil {
		u.OutputTokens = *raw.OutputTokens
		u.Seen = true
	}
	if raw.CacheReadInput != nil {
		u.CacheReadTokens = *raw.CacheReadInput
		u.Seen = true
	}
	if raw.CacheCreation != nil {
		if raw.CacheCreation.Ephemeral5mInput != nil {
			u.CacheCreation5mTokens = *raw.CacheCreation.Ephemeral5mInput
			u.Seen = true
		}
		if raw.CacheCreation.Ephemeral1hInput != nil {
			u.CacheCreation1hTokens = *raw.CacheCreation.Ephemeral1hInput
			u.Seen = true
		}
	} else if raw.CacheCreationInput != nil {
		// The legacy flat field predates TTL detail. Five minutes is Anthropic's
		// default cache duration, so it is the least surprising valuation bucket.
		u.CacheCreation5mTokens = *raw.CacheCreationInput
		u.Seen = true
	}
}

type sseObserver struct {
	line    []byte
	data    []byte
	usage   Usage
	model   string
	stopped bool
}

func (s *sseObserver) write(data []byte) {
	for len(data) > 0 {
		index := bytes.IndexByte(data, '\n')
		if index < 0 {
			s.line = append(s.line, data...)
			return
		}
		s.line = append(s.line, data[:index]...)
		s.consumeLine(bytes.TrimSuffix(s.line, []byte{'\r'}))
		s.line = s.line[:0]
		data = data[index+1:]
	}
}

func (s *sseObserver) consumeLine(line []byte) {
	if len(line) == 0 {
		s.consumeEvent()
		return
	}
	if bytes.HasPrefix(line, []byte("data:")) {
		data := bytes.TrimSpace(line[len("data:"):])
		if len(s.data) != 0 {
			s.data = append(s.data, '\n')
		}
		s.data = append(s.data, data...)
	}
}

func (s *sseObserver) consumeEvent() {
	if len(s.data) == 0 {
		return
	}
	// Content deltas dominate a long stream but never carry billing usage. A
	// cheap byte check avoids unmarshalling every generated text fragment.
	if !bytes.Contains(s.data, []byte("message_start")) &&
		!bytes.Contains(s.data, []byte("message_delta")) &&
		!bytes.Contains(s.data, []byte("message_stop")) {
		s.data = s.data[:0]
		return
	}
	var envelope struct {
		Type    string    `json:"type"`
		Usage   wireUsage `json:"usage"`
		Message *struct {
			Model string    `json:"model"`
			Usage wireUsage `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(s.data, &envelope) == nil {
		switch envelope.Type {
		case "message_start":
			if envelope.Message != nil {
				s.model = envelope.Message.Model
				s.usage.apply(envelope.Message.Usage)
			}
		case "message_delta":
			s.usage.apply(envelope.Usage)
		case "message_stop":
			s.stopped = true
		}
	}
	s.data = s.data[:0]
}

func (s *sseObserver) finish() {
	if len(s.line) != 0 {
		s.consumeLine(bytes.TrimSuffix(s.line, []byte{'\r'}))
		s.line = s.line[:0]
	}
	s.consumeEvent()
}

func parseNonStreaming(tail []byte) (Usage, string, bool) {
	var message struct {
		Model string    `json:"model"`
		Usage wireUsage `json:"usage"`
	}
	if json.Unmarshal(tail, &message) == nil {
		var usage Usage
		usage.apply(message.Usage)
		return usage, message.Model, usage.Seen
	}
	raw, ok := lastJSONObjectField(tail, "usage")
	if !ok {
		return Usage{}, "", false
	}
	var wire wireUsage
	if json.Unmarshal(raw, &wire) != nil {
		return Usage{}, "", false
	}
	var usage Usage
	usage.apply(wire)
	return usage, "", usage.Seen
}

func lastJSONObjectField(data []byte, field string) ([]byte, bool) {
	needle := []byte(`"` + field + `"`)
	start := bytes.LastIndex(data, needle)
	if start < 0 {
		return nil, false
	}
	colon := bytes.IndexByte(data[start+len(needle):], ':')
	if colon < 0 {
		return nil, false
	}
	start += len(needle) + colon + 1
	for start < len(data) && (data[start] == ' ' || data[start] == '\t' || data[start] == '\r' || data[start] == '\n') {
		start++
	}
	if start >= len(data) || data[start] != '{' {
		return nil, false
	}
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(data); index++ {
		char := data[index]
		if inString {
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == '"' {
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return data[start : index+1], true
			}
		}
	}
	return nil, false
}
