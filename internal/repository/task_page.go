package repository

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// PartitionLister lists one page of a task listing within a single partition
// (a shard or a cluster node), resuming after cursor when it is not empty.
type PartitionLister func(ctx context.Context, partition string, limit int, cursor string) (*domain.TaskPage, error)

// partitionCursor is the opaque cursor of a listing that spans partitions:
// the partition to resume in and that partition's own cursor.
type partitionCursor struct {
	Partition string `json:"p"`
	Cursor    string `json:"c,omitempty"`
}

// ListAcrossPartitions returns up to limit tasks of a listing that spans
// partitions, visiting them in the given order and each one from its own
// cursor. The returned cursor names the partition to resume in, so it stays
// valid while the set of partitions is unchanged; a cursor naming an unknown
// partition fails with domain.ErrInvalidCursor. Any partition error fails the
// whole page: a listing never skips a partition silently.
func ListAcrossPartitions(ctx context.Context, partitions []string, limit int, cursor string, list PartitionLister) (*domain.TaskPage, error) {
	start, inner, err := decodePartitionCursor(partitions, cursor)
	if err != nil {
		return nil, err
	}
	page := &domain.TaskPage{Tasks: []*domain.Task{}}
	for i := start; i < len(partitions) && len(page.Tasks) < limit; i++ {
		sub, err := list(ctx, partitions[i], limit-len(page.Tasks), inner)
		if err != nil {
			return nil, err
		}
		inner = ""
		page.Tasks = append(page.Tasks, sub.Tasks...)
		if sub.NextCursor != "" {
			page.NextCursor = encodePartitionCursor(partitions[i], sub.NextCursor)
			return page, nil
		}
		if len(page.Tasks) >= limit && i+1 < len(partitions) {
			page.NextCursor = encodePartitionCursor(partitions[i+1], "")
		}
	}
	return page, nil
}

func encodePartitionCursor(partition, cursor string) string {
	b, _ := json.Marshal(partitionCursor{Partition: partition, Cursor: cursor})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodePartitionCursor(partitions []string, cursor string) (int, string, error) {
	if cursor == "" {
		return 0, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, "", domain.ErrInvalidCursor
	}
	var pc partitionCursor
	if err := json.Unmarshal(raw, &pc); err != nil {
		return 0, "", domain.ErrInvalidCursor
	}
	for i, p := range partitions {
		if p == pc.Partition {
			return i, pc.Cursor, nil
		}
	}
	return 0, "", domain.ErrInvalidCursor
}
