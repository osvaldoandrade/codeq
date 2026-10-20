package raft

// DurableWrites reports the configured local log/stable/FSM fsync contract.
func (d *DB) DurableWrites() bool { return d.cfg.FsyncOnCommit }
