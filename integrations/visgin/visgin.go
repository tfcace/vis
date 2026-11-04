// Package visgin connects vis to gin: every request carries a snapshot of the
// knobs and is timed under its route pattern, as with vis.HTTP. It is its own
// module, so vis itself never depends on gin.
//
//	r := gin.Default()
//	r.Use(visgin.Middleware())
//	r.POST("/api/generate", func(c *gin.Context) {
//		n := MaxLoaded.From(c.Request.Context())
//		…
//	})
//
// A // vis:slo directive above a handler registered this way names its route
// as the middleware does, "POST /api/generate", and vis gen finds it.
package visgin

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/tfcace/vis"
)

// Middleware is MiddlewareIn for the default instance, the one vis.Start serves.
func Middleware() gin.HandlerFunc { return MiddlewareIn(vis.DefaultInstance()) }

// MiddlewareIn puts a snapshot of in's knobs in each request's context and
// times the request under "METHOD /route/:pattern", the way gin's FullPath
// names it. Requests gin can't route count under one route, so paths never
// become routes. Use it after gin.Recovery, as gin.Default has it, so a panic
// counts as the 500 Recovery sends.
func MiddlewareIn(in *vis.Instance) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, end := in.StartRequest(c.Request.Context())
		c.Request = c.Request.WithContext(ctx)
		panicked := true
		defer func() {
			status := c.Writer.Status()
			if panicked {
				status = http.StatusInternalServerError // Recovery, outside us, sends a 500
			}
			end(route(c), status)
		}()
		c.Next()
		panicked = false
	}
}

// route is "METHOD /pattern". Gin leaves FullPath empty when nothing matched,
// and vis counts those under "unmatched".
func route(c *gin.Context) string {
	if c.FullPath() == "" {
		return ""
	}
	return c.Request.Method + " " + c.FullPath()
}
