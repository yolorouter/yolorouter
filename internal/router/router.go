// Package router wires up the Gin engine: health check, embedded frontend
// static assets with SPA fallback, and the /api|/v1 namespace 404 dispatch.
package router

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter-ce/internal/middleware"
	"github.com/yolorouter/yolorouter-ce/internal/web"
	"github.com/yolorouter/yolorouter-ce/pkg/errcode"
)

// isRegularFile reports whether name exists in fsys and is a regular file,
// not a directory — a real Vite build has an assets/ directory, and serving
// a directory path via http.ServeFileFS would list its contents instead of
// falling through to isStaticAssetNamespace's real-404 branch below.
func isRegularFile(fsys fs.FS, name string) bool {
	info, err := fs.Stat(fsys, name)
	return err == nil && !info.IsDir()
}

func New() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.HandleMethodNotAllowed = true

	r.Use(middleware.RequestID())
	r.Use(middleware.AccessLog())
	r.Use(middleware.Recovery())

	healthz := func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
	r.GET("/healthz", healthz)
	r.HEAD("/healthz", healthz) // design doc §10/§14 criterion 7: /healthz accepts GET and HEAD

	distFS, err := fs.Sub(web.DistFS, "dist")
	if err != nil {
		panic("internal/web: dist subtree must exist: " + err.Error())
	}

	// NoMethod covers a wrong-method request against an already-registered
	// route (e.g. POST /healthz); without this, Gin's built-in NoMethod
	// handler would answer with a plain-text 405 instead of a unified
	// envelope. It must still dispatch by namespace the same way NoRoute
	// does below — a wrong-method /v1/* request must get the OpenAI-style
	// shape, not the admin envelope.
	r.NoMethod(func(c *gin.Context) {
		middleware.WriteNamespacedError(c, c.Request.URL.Path, http.StatusMethodNotAllowed, errcode.MethodNotAllowed)
	})

	r.NoRoute(func(c *gin.Context) {
		path := c.Request.URL.Path

		if middleware.IsAdminNamespace(path) || middleware.IsGatewayNamespace(path) {
			middleware.WriteNamespacedError(c, path, http.StatusNotFound, errcode.RouteNotFound)
			return
		}

		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			middleware.WriteAdminError(c, http.StatusMethodNotAllowed, errcode.MethodNotAllowed)
			return
		}

		assetPath := strings.TrimPrefix(path, "/")
		if assetPath == "" {
			assetPath = "index.html"
		}
		if isRegularFile(distFS, assetPath) {
			http.ServeFileFS(c.Writer, c.Request, distFS, assetPath)
			return
		}

		// Requests under the hashed static-asset directory (Vite's
		// build convention, design doc §7) are real asset lookups, not
		// SPA client routes — a miss here must be a genuine 404, or a
		// stale/incorrect asset reference would silently "succeed" by
		// serving index.html instead of surfacing as broken.
		if isStaticAssetNamespace(path) {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}

		// SPA fallback: no matching embedded file, hand off to the
		// frontend router.
		c.Header("Cache-Control", "no-cache")
		http.ServeFileFS(c.Writer, c.Request, distFS, "index.html")
	})

	return r
}

// isStaticAssetNamespace reports whether path falls under the embedded
// frontend's hashed static-asset directory (Vite's `assets/` build output,
// design doc §7). Unlike arbitrary SPA client routes, a miss here is a real
// 404, not an index.html fallback.
func isStaticAssetNamespace(path string) bool {
	return path == "/assets" || strings.HasPrefix(path, "/assets/")
}
