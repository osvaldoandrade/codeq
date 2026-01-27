package domain

type QueueStats struct {
	Command    Command `json:"command"`
	Ready      int64   `json:"ready"`
	Delayed    int64   `json:"delayed"`
	InProgress int64   `json:"inProgress"`
	DLQ        int64   `json:"dlq"`
}
