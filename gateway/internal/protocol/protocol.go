// Package protocol mirrors console/packages/protocol/src/index.ts.
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
)

// Size limits for WebSocket messages.
const (
	MaxTextBytes   = 64 << 10
	MaxBinaryBytes = 256 << 10
)

var streamIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ValidStreamID reports whether s is a valid stream id.
func ValidStreamID(s string) bool { return len(s) >= 1 && len(s) <= 64 && streamIDRe.MatchString(s) }

// Message is any wire message carrying a "type".
type Message interface{ MsgType() string }

type BBox struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

type Actor struct {
	Label      string   `json:"label"`
	TrackID    string   `json:"track_id,omitempty"`
	BBox       *BBox    `json:"bbox,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
}

type FrameRef struct {
	StreamID string `json:"stream_id"`
	TsMs     int64  `json:"ts_ms"`
}

type SceneEvent struct {
	ID         string         `json:"id"`
	StreamID   string         `json:"stream_id"`
	TsMs       int64          `json:"ts_ms"`
	WallMs     int64          `json:"wall_ms"`
	Type       string         `json:"type"`
	Confidence float64        `json:"confidence"`
	Actors     []Actor        `json:"actors"`
	Attrs      map[string]any `json:"attrs"`
	Summary    string         `json:"summary"`
	FrameRef   *FrameRef      `json:"frame_ref,omitempty"`
	Scenario   string         `json:"scenario"`
}

type MinuteSummary struct {
	StreamID      string `json:"stream_id"`
	MinuteStartMs int64  `json:"minute_start_ms"`
	WallMs        int64  `json:"wall_ms"`
	Text          string `json:"text"`
	EventCount    int    `json:"event_count"`
}

type StreamState string

const (
	StateStarting StreamState = "starting"
	StateLive     StreamState = "live"
	StateStalled  StreamState = "stalled"
	StateStopped  StreamState = "stopped"
	StateError    StreamState = "error"
)

type StreamInfo struct {
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	URI             string      `json:"uri"`
	State           StreamState `json:"state"`
	FPS             *float64    `json:"fps,omitempty"`
	LastFrameWallMs *int64      `json:"last_frame_wall_ms,omitempty"`

	// Per-stream health (additive; absent until the first frame).
	LastFrameAgeMs *int64 `json:"last_frame_age_ms,omitempty"`
	FramesTotal    uint64 `json:"frames_total,omitempty"`
	DroppedFrames  uint64 `json:"dropped_frames,omitempty"`
	StoredFrames   *int   `json:"stored_frames,omitempty"`
}

// AudioChunk fields shared by audio.chunk and assistant.audio.
type AudioChunk struct {
	Seq        int64  `json:"seq"`
	TsMs       int64  `json:"ts_ms"`
	SampleRate int    `json:"sample_rate"`
	PCM16B64   string `json:"pcm16_b64"`
}

// ---- client -> server

type SessionStart struct {
	Type      string   `json:"type"`
	Scenario  string   `json:"scenario"`
	StreamIDs []string `json:"stream_ids"`
	Locale    string   `json:"locale"`
}
type ClientAudio struct {
	Type string `json:"type"`
	AudioChunk
}
type TextMessage struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type BargeIn struct {
	Type string `json:"type"`
}
type Stop struct {
	Type string `json:"type"`
}
type HistoryQuery struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	StreamID  string `json:"stream_id,omitempty"`
	SinceMs   int64  `json:"since_ms"`
	UntilMs   int64  `json:"until_ms"`
	Text      string `json:"text,omitempty"`
	Limit     int    `json:"limit"`
}

// ---- server -> client

type SessionStarted struct {
	Type         string       `json:"type"`
	SessionID    string       `json:"session_id"`
	Scenario     string       `json:"scenario"`
	Streams      []StreamInfo `json:"streams"`
	ServerWallMs int64        `json:"server_wall_ms"`
}
type TranscriptPartial struct {
	Type string `json:"type"`
	Text string `json:"text"`
	TsMs int64  `json:"ts_ms"`
}
type TranscriptFinal struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	TsMs        int64  `json:"ts_ms"`
	UtteranceID string `json:"utterance_id"`
}
type AssistantText struct {
	Type        string `json:"type"`
	UtteranceID string `json:"utterance_id"`
	Delta       string `json:"delta"`
	Final       bool   `json:"final"`
}
type AssistantAudio struct {
	Type string `json:"type"`
	AudioChunk
	UtteranceID string `json:"utterance_id"`
}
type AssistantCancelled struct {
	Type        string `json:"type"`
	UtteranceID string `json:"utterance_id"`
	Reason      string `json:"reason"`
}
type EventMessage struct {
	Type  string     `json:"type"`
	Event SceneEvent `json:"event"`
}
type SummaryMessage struct {
	Type    string        `json:"type"`
	Summary MinuteSummary `json:"summary"`
}
type StreamStatus struct {
	Type   string     `json:"type"`
	Stream StreamInfo `json:"stream"`
}
type HistoryResult struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id"`
	Events    []SceneEvent    `json:"events"`
	Summaries []MinuteSummary `json:"summaries"`
	Truncated bool            `json:"truncated"`
}
type ErrorMessage struct {
	Type      string `json:"type"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

func (m SessionStart) MsgType() string       { return "session.start" }
func (m ClientAudio) MsgType() string        { return "audio.chunk" }
func (m TextMessage) MsgType() string        { return "text.message" }
func (m BargeIn) MsgType() string            { return "control.barge_in" }
func (m Stop) MsgType() string               { return "control.stop" }
func (m HistoryQuery) MsgType() string       { return "history.query" }
func (m SessionStarted) MsgType() string     { return "session.started" }
func (m TranscriptPartial) MsgType() string  { return "transcript.partial" }
func (m TranscriptFinal) MsgType() string    { return "transcript.final" }
func (m AssistantText) MsgType() string      { return "assistant.text" }
func (m AssistantAudio) MsgType() string     { return "assistant.audio" }
func (m AssistantCancelled) MsgType() string { return "assistant.cancelled" }
func (m EventMessage) MsgType() string       { return "event" }
func (m SummaryMessage) MsgType() string     { return "summary" }
func (m StreamStatus) MsgType() string       { return "stream.status" }
func (m HistoryResult) MsgType() string      { return "history.result" }
func (m ErrorMessage) MsgType() string       { return "error" }

// Error codes.
const (
	CodeBadRequest   = "bad_request"
	CodeUnauthorized = "unauthorized"
	CodeQuota        = "quota"
	CodeNotFound     = "not_found"
	CodeUpstream     = "upstream"
	CodeInternal     = "internal"
)

// NewError builds an ErrorMessage.
func NewError(code, msg string) ErrorMessage {
	return ErrorMessage{Type: "error", Code: code, Message: msg}
}

// Peek returns the "type" field of a JSON message.
func Peek(data []byte) (string, error) {
	var h struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &h); err != nil {
		return "", err
	}
	if h.Type == "" {
		return "", fmt.Errorf("missing type")
	}
	return h.Type, nil
}

func strict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	return dec.Decode(v)
}

// Decode parses a JSON message into its typed struct (returned by value) and
// applies validation/defaults from the zod schema.
func Decode(data []byte) (Message, error) {
	t, err := Peek(data)
	if err != nil {
		return nil, err
	}
	switch t {
	case "session.start":
		var m SessionStart
		if err := strict(data, &m); err != nil {
			return nil, err
		}
		if m.Locale == "" {
			m.Locale = "en-US"
		}
		if m.Scenario == "" {
			return nil, fmt.Errorf("scenario required")
		}
		if n := len(m.StreamIDs); n < 1 || n > 16 {
			return nil, fmt.Errorf("stream_ids must have 1..16 entries")
		}
		for _, id := range m.StreamIDs {
			if !ValidStreamID(id) {
				return nil, fmt.Errorf("invalid stream id %q", id)
			}
		}
		return m, nil
	case "audio.chunk":
		var m ClientAudio
		if err := strict(data, &m); err != nil {
			return nil, err
		}
		if m.Seq < 0 || m.TsMs < 0 {
			return nil, fmt.Errorf("negative seq/ts_ms")
		}
		switch m.SampleRate {
		case 16000, 24000, 48000:
		default:
			return nil, fmt.Errorf("unsupported sample_rate %d", m.SampleRate)
		}
		return m, nil
	case "text.message":
		var m TextMessage
		if err := strict(data, &m); err != nil {
			return nil, err
		}
		if len(m.Text) < 1 || len([]rune(m.Text)) > 4000 {
			return nil, fmt.Errorf("text length must be 1..4000")
		}
		return m, nil
	case "control.barge_in":
		var m BargeIn
		return m, strict(data, &m)
	case "control.stop":
		var m Stop
		return m, strict(data, &m)
	case "history.query":
		var m HistoryQuery
		if err := strict(data, &m); err != nil {
			return nil, err
		}
		if m.Limit == 0 {
			m.Limit = 50
		}
		if m.Limit < 1 || m.Limit > 200 {
			return nil, fmt.Errorf("limit out of range")
		}
		if m.StreamID != "" && !ValidStreamID(m.StreamID) {
			return nil, fmt.Errorf("invalid stream id")
		}
		if len([]rune(m.Text)) > 500 {
			return nil, fmt.Errorf("text too long")
		}
		return m, nil
	case "session.started":
		var m SessionStarted
		return m, strict(data, &m)
	case "transcript.partial":
		var m TranscriptPartial
		return m, strict(data, &m)
	case "transcript.final":
		var m TranscriptFinal
		return m, strict(data, &m)
	case "assistant.text":
		var m AssistantText
		return m, strict(data, &m)
	case "assistant.audio":
		var m AssistantAudio
		return m, strict(data, &m)
	case "assistant.cancelled":
		var m AssistantCancelled
		return m, strict(data, &m)
	case "event":
		var m EventMessage
		return m, strict(data, &m)
	case "summary":
		var m SummaryMessage
		return m, strict(data, &m)
	case "stream.status":
		var m StreamStatus
		return m, strict(data, &m)
	case "history.result":
		var m HistoryResult
		return m, strict(data, &m)
	case "error":
		var m ErrorMessage
		return m, strict(data, &m)
	}
	return nil, fmt.Errorf("unknown message type %q", t)
}

// IsClientType reports whether t is a client->server message type.
func IsClientType(t string) bool {
	switch t {
	case "session.start", "audio.chunk", "text.message", "control.barge_in", "control.stop", "history.query":
		return true
	}
	return false
}

// REST types.

type CreateStreamRequest struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URI  string `json:"uri"`
}
type CreateSessionRequest struct {
	Scenario  string   `json:"scenario"`
	StreamIDs []string `json:"stream_ids"`
}
type SessionInfo struct {
	ID            string   `json:"id"`
	Scenario      string   `json:"scenario"`
	StreamIDs     []string `json:"stream_ids"`
	CreatedWallMs int64    `json:"created_wall_ms"`
	State         string   `json:"state"`
}
