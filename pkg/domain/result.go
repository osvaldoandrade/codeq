package domain

import "time"

type SubmitResultRequest struct {
	WorkerID  string         `json:"workerId,omitempty"`
	Status    TaskStatus     `json:"status" binding:"required"`
	Result    map[string]any `json:"result,omitempty"`
	Error     string         `json:"error,omitempty"`
	Artifacts []ArtifactIn   `json:"artifacts,omitempty"`
}

type ArtifactIn struct {
	Name          string `json:"name" binding:"required"`
	URL           string `json:"url,omitempty"`
	ContentBase64 string `json:"contentBase64,omitempty"`
	ContentType   string `json:"contentType,omitempty"`
}

type ArtifactOut struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type ResultRecord struct {
	TaskID      string         `json:"taskId"`
	Status      TaskStatus     `json:"status"`
	Result      map[string]any `json:"result,omitempty"`
	Error       string         `json:"error,omitempty"`
	Artifacts   []ArtifactOut  `json:"artifacts,omitempty"`
	CompletedAt time.Time      `json:"completedAt"`
}
