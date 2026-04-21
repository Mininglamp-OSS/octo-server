package voice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

// ASRInput holds the original input parameters
type ASRInput struct {
	Mode        string `json:"mode"`
	MimeType    string `json:"mime_type"`
	AudioSize   int    `json:"audio_size"`
	AudioFile   string `json:"audio_file"`
	ContextText     string `json:"context_text"`
	ChatContext     string `json:"chat_context"`
	PersonalContext string `json:"personal_context"`
	MemberContext   string `json:"member_context"`
	Model           string `json:"model"`
	Language    string `json:"language"`
}

// ASRPrompt holds the prompt and request body sent to the model
type ASRPrompt struct {
	Type        string      `json:"type"`
	Text        string      `json:"text"`
	RequestBody interface{} `json:"request_body"`
}

// ASREntry holds metadata for a single ASR request
type ASREntry struct {
	RequestID      string     `json:"request_id"`
	Timestamp      string     `json:"timestamp"`
	Source         string     `json:"source"`
	Engine         string     `json:"engine"`
	ModelRequested string     `json:"model_requested"`
	ModelUsed      string     `json:"model_used"`
	Input          ASRInput   `json:"input"`
	Prompt         *ASRPrompt `json:"prompt,omitempty"`
	RawResultText  string     `json:"raw_result_text"`
	ResultText     string     `json:"result_text"`
	ResultLength   int        `json:"result_length"`
	IsNoSpeech     bool       `json:"is_no_speech"`
	Error          string     `json:"error"`
	DurationMs     int64      `json:"duration_ms"`
	PodID          string     `json:"pod_id"`

	// AudioData is used internally for writing the audio file, not serialized to JSON
	AudioData []byte `json:"-"`
}

// ASRLogger writes ASR data (JSON + audio file) to disk asynchronously
type ASRLogger struct {
	baseDir string
	podID   string
	ch      chan ASREntry
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	log.Log
}

// NewASRLogger creates a logger that writes to baseDir.
// bufSize controls the async channel buffer size.
// Returns nil if the base directory cannot be created (ASR logging will be disabled).
func NewASRLogger(baseDir string, bufSize int) *ASRLogger {
	podID := os.Getenv("HOSTNAME")
	if podID == "" {
		podID = "local"
	}

	if err := os.MkdirAll(baseDir, 0755); err != nil {
		lg := log.NewTLog("ASRLog")
		lg.Error("ASR_LOG_DIR is not writable, ASR logging disabled",
			zap.String("dir", baseDir), zap.Error(err))
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())

	l := &ASRLogger{
		baseDir: baseDir,
		podID:   podID,
		ch:      make(chan ASREntry, bufSize),
		ctx:     ctx,
		cancel:  cancel,
		Log:     log.NewTLog("ASRLog"),
	}
	l.wg.Add(1)
	go l.worker()
	return l
}

// Close stops the logger gracefully: cancels ctx so worker drains remaining
// entries and exits, then waits for worker to finish.
func (l *ASRLogger) Close() {
	l.cancel()
	l.wg.Wait()
}

// Enqueue enqueues an entry for async writing. Drops silently if buffer full or logger closed.
func (l *ASRLogger) Enqueue(entry ASREntry) {
	if l.ctx.Err() != nil {
		return
	}
	entry.PodID = l.podID
	select {
	case l.ch <- entry:
	case <-l.ctx.Done():
	default:
		l.Warn("asr log buffer full, dropping entry",
			zap.String("request_id", entry.RequestID))
	}
}

// GenerateRequestID creates a unique request ID: {pod}_{unixMs}_{rand6}
func (l *ASRLogger) GenerateRequestID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return fmt.Sprintf("%s_%d_%s", l.podID,
		time.Now().UTC().UnixMilli(),
		hex.EncodeToString(b))
}

func (l *ASRLogger) worker() {
	defer l.wg.Done()

	for {
		select {
		case entry := <-l.ch:
			l.writeEntry(entry)
		case <-l.ctx.Done():
			goto drain
		}
	}

drain:
	for {
		select {
		case entry := <-l.ch:
			l.writeEntry(entry)
		default:
			return
		}
	}
}

func (l *ASRLogger) writeEntry(entry ASREntry) {
	now, _ := time.Parse(time.RFC3339Nano, entry.Timestamp)
	if now.IsZero() {
		now = time.Now().UTC()
	}

	dateDir := now.UTC().Format("2006-01-02")
	dir := filepath.Join(l.baseDir, dateDir, entry.Engine)

	if err := os.MkdirAll(dir, 0755); err != nil {
		l.Error("mkdir failed", zap.Error(err), zap.String("dir", dir))
		return
	}

	// Write independent audio file first, then JSON
	if len(entry.AudioData) > 0 {
		ext := mimeTypeToFormat(entry.Input.MimeType)
		audioFileName := entry.RequestID + "." + ext
		audioPath := filepath.Join(dir, audioFileName)
		if err := os.WriteFile(audioPath, entry.AudioData, 0644); err != nil {
			l.Error("write audio file failed", zap.Error(err),
				zap.String("path", audioPath))
		}
		entry.Input.AudioFile = audioFileName
	}

	jsonBytes, err := json.Marshal(entry)
	if err != nil {
		l.Error("marshal metadata failed", zap.Error(err))
		return
	}
	jsonPath := filepath.Join(dir, entry.RequestID+".json")
	if err := os.WriteFile(jsonPath, jsonBytes, 0644); err != nil {
		l.Error("write metadata failed", zap.Error(err))
	}
}
