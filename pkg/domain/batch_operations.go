package domain

// TaskCompleteUpdate represents a task completion update for batch operations
type TaskCompleteUpdate struct {
	ID       string
	Status   TaskStatus
	ErrorMsg string
}

// TaskDeleteInfo represents task deletion info for batch cleanup operations
type TaskDeleteInfo struct {
	ID      string
	Command Command
}

// BatchSubmitItem represents a single item in a batch submit request
type BatchSubmitItem struct {
	TaskID              string
	SubmitResultRequest SubmitResultRequest
}

// BatchSubmitResponse represents the response for a single batch submit item
type BatchSubmitResponse struct {
	TaskID string
	Result *ResultRecord
	Error  string
}
