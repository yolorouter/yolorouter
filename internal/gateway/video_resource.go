package gateway

// The caller-facing job resource routes: GET /v1/videos/{id} (poll) and
// GET /v1/videos/{id}/content (download). These are not relay requests —
// nothing routes to a provider here — so they read the task domain
// directly: ownership first, then the lazy poll the task service owns,
// then the dialect's job resource rendering. The content route proxies
// bytes from the upstream's own result URL: this gateway stores no media
// (rehosting was ruled out), so the caller downloads through us from
// wherever the upstream put it, for as long as the upstream keeps it.

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols/videos"
	"github.com/yolorouter/yolorouter/internal/repository"
)

// GetVideoResource handles GET /v1/videos/{id}: the job in the dialect's
// shape, refreshed from upstream if the poll interval has elapsed. A
// foreign or unknown id is the same 404, so existence is not confirmed
// across keys.
func GetVideoResource(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		proto := IngressProtocolForContext(c)
		rid := requestIDFor(c)
		apiKey, ok := gatewayAPIKeyFromContext(c)
		if !ok {
			WriteIngressError(c, proto, http.StatusInternalServerError, errTypeServer, "missing gateway auth context", rid)
			return
		}
		if rejectInvalidKey(c, proto, apiKey, rid) {
			return
		}
		task, err := svc.videoTasks.Get(c.Request.Context(), apiKey.ID, c.Param("id"), time.Now())
		if errors.Is(err, repository.ErrVideoTaskNotFound) {
			WriteIngressError(c, proto, http.StatusNotFound, errTypeInvalidRequest, "no such video job", rid)
			return
		}
		if err != nil {
			WriteIngressError(c, proto, http.StatusInternalServerError, errTypeServer, "internal error", rid)
			return
		}
		c.JSON(http.StatusOK, renderVideoResource(task))
	}
}

// resultURLWindow is how long a completed task's upstream result URL
// lives — the vendors this build polls all document 24 hours. The wire's
// expires_at reports it so a caller knows how long /content will answer.
const resultURLWindow = 24 * time.Hour

// renderVideoResource maps a task onto the wire's job shape. The status
// vocabulary is the four the SDK's strict typing accepts; the internal
// cancelled and expired states travel as failed plus the error channel,
// which is how the caller learns which one it was.
func renderVideoResource(task *model.VideoTask) videos.Resource {
	wire, wireErrCode := videos.MapWireStatus(task.Status)
	res := videos.Resource{
		ID: task.ID, Object: "video", Model: task.ModelName,
		Status: wire, Progress: 0,
		CreatedAt: task.CreatedAt.Unix(),
		Size:      task.Size, Seconds: strconv.Itoa(task.Seconds),
	}
	if task.UpstreamCompletedAt != nil {
		unix := task.UpstreamCompletedAt.Unix()
		res.CompletedAt = &unix
		if task.Status == model.VideoTaskCompleted {
			expires := task.UpstreamCompletedAt.Add(resultURLWindow).Unix()
			res.ExpiresAt = &expires
		}
	}
	if task.Status == model.VideoTaskFailed || wireErrCode != "" {
		code := task.ErrorCode
		if code == "" {
			code = wireErrCode
		}
		res.Error = &videos.ResourceError{Code: code, Message: task.ErrorMessage}
	}
	return res
}

// GetVideoContent handles GET /v1/videos/{id}/content: the finished
// video's bytes, proxied from the upstream's result URL. An unfinished
// job, a failed one, or a completed one whose upstream URL has since died
// are all a 404-shaped miss — the SDK's download path treats them alike.
func GetVideoContent(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		proto := IngressProtocolForContext(c)
		rid := requestIDFor(c)
		apiKey, ok := gatewayAPIKeyFromContext(c)
		if !ok {
			WriteIngressError(c, proto, http.StatusInternalServerError, errTypeServer, "missing gateway auth context", rid)
			return
		}
		if rejectInvalidKey(c, proto, apiKey, rid) {
			return
		}
		task, err := svc.videoTasks.Get(c.Request.Context(), apiKey.ID, c.Param("id"), time.Now())
		if errors.Is(err, repository.ErrVideoTaskNotFound) {
			WriteIngressError(c, proto, http.StatusNotFound, errTypeInvalidRequest, "no downloadable video for this job", rid)
			return
		}
		if err != nil {
			// Same split as the resource route: a miss is the caller's
			// 404, a database failure is the gateway's 500 — merging them
			// would read an outage as "nothing to download".
			WriteIngressError(c, proto, http.StatusInternalServerError, errTypeServer, "internal error", rid)
			return
		}
		if task.Status != model.VideoTaskCompleted || task.ResultURL == "" {
			WriteIngressError(c, proto, http.StatusNotFound, errTypeInvalidRequest, "no downloadable video for this job", rid)
			return
		}
		svc.proxyVideoContent(c, task.ResultURL)
	}
}

// proxyVideoContent streams the upstream's bytes to the caller without
// buffering them: a video is megabytes and the caller asked to download
// it, not to hold it in this process twice.
func (s *Service) proxyVideoContent(c *gin.Context, url string) {
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, url, nil)
	if err != nil {
		WriteIngressError(c, IngressProtocolForContext(c), http.StatusInternalServerError, errTypeServer, "internal error", requestIDFor(c))
		return
	}
	resp, err := s.client.SendUpstreamRequest(req)
	if err != nil {
		WriteIngressError(c, IngressProtocolForContext(c), http.StatusBadGateway, errTypeUpstream, "video source unreachable", requestIDFor(c))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		WriteIngressError(c, IngressProtocolForContext(c), http.StatusNotFound, errTypeInvalidRequest, "video source expired", requestIDFor(c))
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Header("Content-Type", ct)
	} else {
		c.Header("Content-Type", "video/mp4")
	}
	c.Status(http.StatusOK)
	flusher, canFlush := c.Writer.(http.Flusher)
	_, _ = io.Copy(c.Writer, resp.Body)
	if canFlush {
		flusher.Flush()
	}
}
