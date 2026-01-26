package domain

import (
	"encoding"
	"time"
)

type Command string

const (
	CmdGenerateMaster   Command = "GENERATE_MASTER"
	CmdGenerateCreative Command = "GENERATE_CREATIVE"
)

type TaskStatus string

const (
	StatusPending    TaskStatus = "PENDING"
	StatusInProgress TaskStatus = "IN_PROGRESS"
	StatusCompleted  TaskStatus = "COMPLETED"
	StatusFailed     TaskStatus = "FAILED"
)

type Task struct {
	ID          string     `json:"id"`
	Command     Command    `json:"command"`
	Payload     string     `json:"payload"` // JSON string opaco
	Priority    int        `json:"priority,omitempty"`
	Webhook     string     `json:"webhook,omitempty"`
	Status      TaskStatus `json:"status"`
	WorkerID    string     `json:"workerId,omitempty"`
	LeaseUntil  string     `json:"leaseUntil,omitempty"` // RFC3339
	Attempts    int        `json:"attempts,omitempty"`
	MaxAttempts int        `json:"maxAttempts,omitempty"`
	Error       string     `json:"error,omitempty"`
	ResultKey   string     `json:"resultKey,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

var (
	_ encoding.BinaryMarshaler = Command("")
	_ encoding.TextMarshaler   = Command("")
	_ encoding.BinaryMarshaler = TaskStatus("")
	_ encoding.TextMarshaler   = TaskStatus("")
)

func (c Command) MarshalBinary() ([]byte, error) { return []byte(string(c)), nil }
func (c Command) MarshalText() ([]byte, error)   { return []byte(string(c)), nil }

func (s TaskStatus) MarshalBinary() ([]byte, error) { return []byte(string(s)), nil }
func (s TaskStatus) MarshalText() ([]byte, error)   { return []byte(string(s)), nil }
