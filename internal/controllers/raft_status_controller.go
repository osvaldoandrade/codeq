package controllers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// RaftGroupStatus is the local mirror of the pkg/app.RaftGroupStatus
// interface, kept here to avoid importing pkg/app from internal/.
// Both types satisfy each other structurally because Go interfaces
// are nominal-by-method.
type RaftGroupStatus interface {
	IsLeader() bool
	SelfID() string
	BindAddr() string
	LeaderInfo() (id, addr string)
}

// raftStatusController exposes the local node's per-shard raft state.
// In raft mode there is one RaftGroupStatus per Pebble shard; without
// raft the controller returns enabled=false and an empty groups list.
type raftStatusController struct {
	groups []RaftGroupStatus
}

// NewRaftStatusController returns a controller that surfaces the given
// per-shard raft groups. Pass nil/empty for non-raft deployments.
func NewRaftStatusController(groups []RaftGroupStatus) *raftStatusController {
	return &raftStatusController{groups: groups}
}

type raftGroupView struct {
	ShardIdx    int    `json:"shardIdx"`
	IsLeader    bool   `json:"isLeader"`
	SelfID      string `json:"selfId"`
	SelfAddr    string `json:"selfAddr"`
	LeaderID    string `json:"leaderId"`
	LeaderAddr  string `json:"leaderAddr"`
	HasLeader   bool   `json:"hasLeader"`
}

type raftStatusResponse struct {
	Enabled   bool             `json:"enabled"`
	NumGroups int              `json:"numGroups"`
	Groups    []raftGroupView  `json:"groups,omitempty"`
}

// Handle answers GET /v1/codeq/raft/status. Returns 200 always — the
// payload is the source of truth (enabled=false ⇒ no raft on this node).
func (h *raftStatusController) Handle(c *gin.Context) {
	resp := raftStatusResponse{
		Enabled:   len(h.groups) > 0,
		NumGroups: len(h.groups),
	}
	for i, g := range h.groups {
		leaderID, leaderAddr := g.LeaderInfo()
		resp.Groups = append(resp.Groups, raftGroupView{
			ShardIdx:   i,
			IsLeader:   g.IsLeader(),
			SelfID:     g.SelfID(),
			SelfAddr:   g.BindAddr(),
			LeaderID:   leaderID,
			LeaderAddr: leaderAddr,
			HasLeader:  leaderID != "",
		})
	}
	c.JSON(http.StatusOK, resp)
}
