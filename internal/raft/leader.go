package raft

import "sync"

// LeaseTable is a placeholder for the in-memory lease table that the
// repository layer expects on the DB wrapper. M1 keeps it minimal —
// task_repository.go uses it as a typed reference; T6 either wires the
// real internal/repository/pebble/lease_table.go or moves that code
// here (decided when delegation pattern is wired).
type LeaseTable struct {
	mu sync.RWMutex
}

func newLeaseTable() *LeaseTable {
	return &LeaseTable{}
}
