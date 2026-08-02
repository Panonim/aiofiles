// Package jobs defines the job model, persistence, event bus and worker pool.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

type Type string

const (
	TypeDownload Type = "download" // yt-dlp
	TypeConvert  Type = "convert"  // ffmpeg, format/codec change
	TypeCompress Type = "compress" // ffmpeg, size reduction
	TypeImage    Type = "image"    // ImageMagick
)

func (t Type) Valid() bool {
	switch t {
	case TypeDownload, TypeConvert, TypeCompress, TypeImage:
		return true
	}
	return false
}

type Status string

const (
	StatusQueued   Status = "queued"
	StatusRunning  Status = "running"
	StatusDone     Status = "done"
	StatusFailed   Status = "failed"
	StatusCanceled Status = "canceled"
	StatusExpired  Status = "expired"
)

func (s Status) Terminal() bool {
	switch s {
	case StatusDone, StatusFailed, StatusCanceled, StatusExpired:
		return true
	}
	return false
}

// Job is one unit of work.
type Job struct {
	ID         string          `json:"id"`
	Type       Type            `json:"type"`
	Title      string          `json:"title"`
	Source     string          `json:"source"`      // URL for downloads, original filename otherwise
	Params     json.RawMessage `json:"params"`      // allow-list-validated payload, see internal/presets
	InputPath  string          `json:"-"`           // uploaded file on disk, if any
	OutputPath string          `json:"-"`           // absolute path, never exposed to clients
	OutputName string          `json:"output_name"` // basename shown in the UI
	OutputSize int64           `json:"output_size"`
	Status     Status          `json:"status"`
	Progress   float64         `json:"progress"` // 0..100
	Stage      string          `json:"stage"`
	Error      string          `json:"error,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	ExpiresAt  *time.Time      `json:"expires_at,omitempty"`
}

type Progress struct {
	Percent float64 `json:"percent"` // 0..100, negative means "unknown"
	Stage   string  `json:"stage"`   // short human label: "downloading", "encoding"
	Speed   string  `json:"speed,omitempty"`
	ETA     string  `json:"eta,omitempty"`
}

type Result struct {
	OutputPath string // absolute path to the finished file
	Title      string // overrides Job.Title when non-empty
}

// Emit must be non-blocking; it is called from the runner's goroutine.
type Emit func(Progress)

// Runner executes one job type. Implementations build argv slices only; they
// must never interpolate user input into a shell string.
type Runner interface {
	Run(ctx context.Context, j *Job, emit Emit) (Result, error)
}

func NewID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("jobs: entropy source failed: %v", err))
	}
	return hex.EncodeToString(b[:])
}
