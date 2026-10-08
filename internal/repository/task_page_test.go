package repository

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// fakePartitions serves partitions holding sizes[i] tasks named "<i>-<n>",
// with a numeric offset as the partition-local cursor.
func fakePartitions(sizes map[string]int) PartitionLister {
	return func(_ context.Context, partition string, limit int, cursor string) (*domain.TaskPage, error) {
		start := 0
		if cursor != "" {
			start, _ = strconv.Atoi(cursor)
		}
		page := &domain.TaskPage{Tasks: []*domain.Task{}}
		n := sizes[partition]
		for i := start; i < n && len(page.Tasks) < limit; i++ {
			page.Tasks = append(page.Tasks, &domain.Task{ID: partition + "-" + strconv.Itoa(i)})
			if len(page.Tasks) == limit && i+1 < n {
				page.NextCursor = strconv.Itoa(i + 1)
			}
		}
		return page, nil
	}
}

func TestListAcrossPartitionsVisitsEveryTaskOnce(t *testing.T) {
	partitions := []string{"a", "b", "c", "d"}
	sizes := map[string]int{"a": 3, "b": 0, "c": 4, "d": 1}
	for limit := 1; limit <= 9; limit++ {
		var got []string
		cursor := ""
		for pages := 0; ; pages++ {
			if pages > 20 {
				t.Fatalf("limit %d: listing never ended", limit)
			}
			page, err := ListAcrossPartitions(context.Background(), partitions, limit, cursor, fakePartitions(sizes))
			if err != nil {
				t.Fatalf("limit %d: %v", limit, err)
			}
			if len(page.Tasks) > limit {
				t.Fatalf("limit %d: page of %d", limit, len(page.Tasks))
			}
			for _, task := range page.Tasks {
				got = append(got, task.ID)
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		want := []string{"a-0", "a-1", "a-2", "c-0", "c-1", "c-2", "c-3", "d-0"}
		if len(got) != len(want) {
			t.Fatalf("limit %d: got %v, want %v", limit, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("limit %d: got %v, want %v", limit, got, want)
			}
		}
	}
}

func TestListAcrossPartitionsRejectsUnknownCursorAndPropagatesErrors(t *testing.T) {
	ctx := context.Background()
	for _, cursor := range []string{"%%%", "bm90LWpzb24", encodePartitionCursor("z", "")} {
		if _, err := ListAcrossPartitions(ctx, []string{"a"}, 5, cursor, fakePartitions(nil)); !errors.Is(err, domain.ErrInvalidCursor) {
			t.Fatalf("cursor %q: err %v, want ErrInvalidCursor", cursor, err)
		}
	}
	boom := errors.New("node down")
	failing := func(_ context.Context, partition string, limit int, cursor string) (*domain.TaskPage, error) {
		if partition == "b" {
			return nil, boom
		}
		return fakePartitions(map[string]int{"a": 1})(context.Background(), partition, limit, cursor)
	}
	if _, err := ListAcrossPartitions(ctx, []string{"a", "b"}, 5, "", failing); !errors.Is(err, boom) {
		t.Fatalf("partition error: got %v, want it propagated", err)
	}
}
