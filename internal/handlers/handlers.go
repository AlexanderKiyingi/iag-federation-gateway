// Package handlers implements the federation-gateway HTTP API.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/alvor-technologies/iag-platform-go/apierr"
	"github.com/iag/federation-gateway/internal/auth"
	"github.com/iag/federation-gateway/internal/config"
	"github.com/iag/federation-gateway/internal/events"
	"github.com/iag/federation-gateway/internal/models"
	"github.com/iag/federation-gateway/internal/outbox"
	"github.com/iag/federation-gateway/internal/store"
)

// API bundles the handler dependencies.
type API struct {
	Cfg    config.Config
	Store  *store.Store
	Events *events.Bus
	Outbox *outbox.Store
}

func (a *API) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "service": a.Cfg.ServiceName})
}

func (a *API) Ready(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	if err := a.Store.Ping(ctx); err != nil {
		apierr.Write(c, http.StatusServiceUnavailable, apierr.CodeServiceUnavailable, "not ready")
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}

// Status summarises the federation for operators.
func (a *API) Status(c *gin.Context) {
	stats, err := a.Store.Stats(c.Request.Context())
	if err != nil {
		apierr.Internal(c, "stats unavailable")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"service":          a.Cfg.ServiceName,
		"conflictStrategy": string(a.Cfg.ConflictStrategy),
		"stats":            stats,
	})
}

// ------------------------------------------------------------------ nodes

type registerNodeRequest struct {
	NodeID string `json:"nodeId"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
}

// RegisterNode registers or heartbeats an edge node.
func (a *API) RegisterNode(c *gin.Context) {
	var req registerNodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierr.BadRequest(c, "invalid JSON body")
		return
	}
	req.NodeID = strings.TrimSpace(req.NodeID)
	if req.NodeID == "" {
		apierr.BadRequest(c, "nodeId is required")
		return
	}
	node, err := a.Store.RegisterNode(c.Request.Context(), models.Node{
		NodeID: req.NodeID, Name: strings.TrimSpace(req.Name), Kind: strings.TrimSpace(req.Kind),
	})
	if err != nil {
		apierr.Internal(c, "register node failed")
		return
	}
	_ = a.Events.Publish(c.Request.Context(), events.TypeNodeRegistered, node.NodeID, gin.H{
		"nodeId": node.NodeID, "kind": node.Kind, "status": string(node.Status),
	})
	c.JSON(http.StatusOK, node)
}

func (a *API) ListNodes(c *gin.Context) {
	nodes, err := a.Store.ListNodes(c.Request.Context())
	if err != nil {
		apierr.Internal(c, "list nodes failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": nodes, "count": len(nodes)})
}

func (a *API) GetNode(c *gin.Context) {
	node, err := a.Store.GetNode(c.Request.Context(), c.Param("nodeId"))
	if errors.Is(err, store.ErrNotFound) {
		apierr.NotFound(c, "node not found")
		return
	}
	if err != nil {
		apierr.Internal(c, "get node failed")
		return
	}
	c.JSON(http.StatusOK, node)
}

type nodeStatusRequest struct {
	Status string `json:"status"`
}

// SetNodeStatus suspends or reactivates a node.
func (a *API) SetNodeStatus(c *gin.Context) {
	var req nodeStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierr.BadRequest(c, "invalid JSON body")
		return
	}
	status := models.NodeStatus(strings.ToLower(strings.TrimSpace(req.Status)))
	switch status {
	case models.NodeActive, models.NodeInactive, models.NodeSuspended:
	default:
		apierr.BadRequest(c, "status must be active, inactive or suspended")
		return
	}
	node, err := a.Store.SetNodeStatus(c.Request.Context(), c.Param("nodeId"), status)
	if errors.Is(err, store.ErrNotFound) {
		apierr.NotFound(c, "node not found")
		return
	}
	if err != nil {
		apierr.Internal(c, "update node failed")
		return
	}
	c.JSON(http.StatusOK, node)
}

// ------------------------------------------------------------------ sync

type pushRequest struct {
	NodeID  string          `json:"nodeId"`
	Changes []models.Change `json:"changes"`
}

// Push applies a batch of changes from an edge node.
func (a *API) Push(c *gin.Context) {
	var req pushRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierr.BadRequest(c, "invalid JSON body")
		return
	}
	req.NodeID = strings.TrimSpace(req.NodeID)
	if req.NodeID == "" {
		apierr.BadRequest(c, "nodeId is required")
		return
	}
	if len(req.Changes) == 0 {
		apierr.BadRequest(c, "changes must not be empty")
		return
	}
	if len(req.Changes) > a.Cfg.MaxPushBatch {
		apierr.WriteWith(c, http.StatusRequestEntityTooLarge, apierr.CodeBadRequest,
			"too many changes in one push", gin.H{"max": a.Cfg.MaxPushBatch, "got": len(req.Changes)})
		return
	}

	results, err := a.Store.ApplyPush(c.Request.Context(), req.NodeID, req.Changes, a.Cfg.ConflictStrategy, a.Outbox)
	switch {
	case errors.Is(err, store.ErrNotFound):
		apierr.NotFound(c, "node is not registered; call POST /v1/nodes/register first")
		return
	case errors.Is(err, store.ErrNodeNotAllowed):
		apierr.Forbidden(c, err.Error())
		return
	case err != nil:
		apierr.Internal(c, "push failed")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"results":  results,
		"accepted": len(results),
		"summary":  summarise(results),
	})
}

// summarise counts outcomes so a node can log one line instead of walking the
// whole result array.
func summarise(results []models.PushResult) map[string]int {
	out := map[string]int{}
	for _, r := range results {
		out[string(r.Outcome)]++
	}
	return out
}

// Pull returns log entries after a cursor.
func (a *API) Pull(c *gin.Context) {
	after, err := strconv.ParseInt(c.DefaultQuery("cursor", "0"), 10, 64)
	if err != nil || after < 0 {
		apierr.BadRequest(c, "cursor must be a non-negative integer")
		return
	}
	limit := a.Cfg.MaxPullBatch
	if v := strings.TrimSpace(c.Query("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			apierr.BadRequest(c, "limit must be a positive integer")
			return
		}
		if n < limit {
			limit = n
		}
	}
	nodeID := strings.TrimSpace(c.Query("nodeId"))

	entries, err := a.Store.Pull(c.Request.Context(), after, limit, nodeID)
	if err != nil {
		apierr.Internal(c, "pull failed")
		return
	}
	next := after
	if n := len(entries); n > 0 {
		next = entries[n-1].Cursor
	}
	c.JSON(http.StatusOK, gin.H{
		"items":      entries,
		"count":      len(entries),
		"nextCursor": next,
		// hasMore lets a node keep pulling without guessing from a short page.
		"hasMore": len(entries) == limit,
	})
}

type ackRequest struct {
	NodeID string `json:"nodeId"`
	Cursor int64  `json:"cursor"`
}

// Ack records a node's consumed watermark.
func (a *API) Ack(c *gin.Context) {
	var req ackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierr.BadRequest(c, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.NodeID) == "" {
		apierr.BadRequest(c, "nodeId is required")
		return
	}
	if req.Cursor < 0 {
		apierr.BadRequest(c, "cursor must be non-negative")
		return
	}
	if err := a.Store.AckCursor(c.Request.Context(), req.NodeID, req.Cursor); err != nil {
		apierr.Internal(c, "ack failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "cursor": req.Cursor})
}

// ------------------------------------------------------------------ resources

func (a *API) GetResource(c *gin.Context) {
	r, err := a.Store.GetResource(c.Request.Context(),
		strings.ToLower(c.Param("type")), c.Param("id"))
	if errors.Is(err, store.ErrNotFound) {
		apierr.NotFound(c, "resource not found")
		return
	}
	if err != nil {
		apierr.Internal(c, "get resource failed")
		return
	}
	c.JSON(http.StatusOK, r)
}

// ------------------------------------------------------------------ conflicts

func (a *API) ListConflicts(c *gin.Context) {
	state := strings.ToLower(strings.TrimSpace(c.Query("state")))
	switch state {
	case "", string(models.ConflictPending), string(models.ConflictResolved):
	default:
		apierr.BadRequest(c, "state must be pending or resolved")
		return
	}
	limit := 100
	if v := strings.TrimSpace(c.Query("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	items, err := a.Store.ListConflicts(c.Request.Context(), state, limit)
	if err != nil {
		apierr.Internal(c, "list conflicts failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "count": len(items)})
}

func (a *API) GetConflict(c *gin.Context) {
	conflict, err := a.Store.GetConflict(c.Request.Context(), c.Param("id"))
	if errors.Is(err, store.ErrNotFound) {
		apierr.NotFound(c, "conflict not found")
		return
	}
	if err != nil {
		apierr.Internal(c, "get conflict failed")
		return
	}
	c.JSON(http.StatusOK, conflict)
}

type resolveRequest struct {
	Resolution string          `json:"resolution"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// ResolveConflict settles a parked conflict.
func (a *API) ResolveConflict(c *gin.Context) {
	var req resolveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierr.BadRequest(c, "invalid JSON body")
		return
	}
	resolution := models.Resolution(strings.ToLower(strings.TrimSpace(req.Resolution)))
	if !resolution.Valid() {
		apierr.BadRequest(c, "resolution must be keep_server, keep_node or merged")
		return
	}
	if resolution == models.ResolveMerged && len(req.Payload) == 0 {
		apierr.BadRequest(c, "merged resolution requires a payload")
		return
	}

	conflict, err := a.Store.ResolveConflict(c.Request.Context(), c.Param("id"),
		resolution, req.Payload, auth.ActorName(c), a.Outbox)
	if errors.Is(err, store.ErrNotFound) {
		apierr.NotFound(c, "conflict not found")
		return
	}
	if err != nil {
		// An already-resolved conflict is a client mistake (or a double-submit),
		// not a server fault — say so rather than returning a 500.
		if strings.Contains(err.Error(), "already resolved") {
			apierr.WriteWith(c, http.StatusConflict, apierr.CodeConflict, err.Error(), nil)
			return
		}
		apierr.Internal(c, "resolve failed")
		return
	}
	c.JSON(http.StatusOK, conflict)
}
