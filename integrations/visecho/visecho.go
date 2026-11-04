// Package visecho connects vis to echo: every request carries a snapshot of
// the knobs and is timed under its route pattern, as with vis's net/http
// middleware. It is its own module, so vis itself never depends on echo.
//
//	e := echo.New()
//	e.Use(middleware.Recover())
//	e.Use(visecho.Middleware())
//	e.GET("/k/:key", func(c echo.Context) error {
//		n := BatchSize.From(c.Request().Context())
//		…
//	})
package visecho

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/tfcace/vis"
)

// Middleware is MiddlewareIn for the default instance, the one vis.Start serves.
func Middleware() echo.MiddlewareFunc { return MiddlewareIn(vis.DefaultInstance()) }

// MiddlewareIn puts a snapshot of in's knobs in each request's context and
// times the request under "METHOD /route/:pattern". Requests echo can't route
// count under one route, so paths never become routes.
func MiddlewareIn(in *vis.Instance) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) (err error) {
			ctx, end := in.StartRequest(c.Request().Context())
			c.SetRequest(c.Request().WithContext(ctx))
			panicked := true
			defer func() {
				status := c.Response().Status
				if panicked {
					status = http.StatusInternalServerError // Recover, outside us, turns it into a 500
				}
				end(route(c), status)
			}()
			err = next(c)
			panicked = false
			if err != nil {
				// Let echo write the error now, as its own logger middleware does, so
				// the status we record is the one sent. Returning err keeps outer
				// middleware informed; echo won't write a committed response twice.
				c.Error(err)
			}
			return err
		}
	}
}

// route is "METHOD /pattern". Echo leaves the path empty when nothing
// matched, and vis counts those under "unmatched".
func route(c echo.Context) string {
	if c.Path() == "" {
		return ""
	}
	return c.Request().Method + " " + c.Path()
}
