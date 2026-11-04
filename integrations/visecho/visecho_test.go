package visecho_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/tfcace/vis"
	"github.com/tfcace/vis/integrations/visecho"
)

var limit = vis.NewInt("echo_test_limit", vis.Range(1, 100), vis.Default(10))

func newServer(in *vis.Instance) *echo.Echo {
	e := echo.New()
	e.Use(middleware.Recover()) // outermost, as most echo apps have it
	e.Use(visecho.MiddlewareIn(in))
	e.GET("/k/:key", func(c echo.Context) error {
		return c.String(http.StatusOK, fmt.Sprint(limit.From(c.Request().Context())))
	})
	e.GET("/fail", func(c echo.Context) error { return echo.NewHTTPError(http.StatusBadGateway, "origin down") })
	e.GET("/boom", func(c echo.Context) error { panic("boom") })
	return e
}

func get(e *echo.Echo, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestEchoHandlersReadKnobs(t *testing.T) {
	in := vis.NewInstance()
	if _, err := in.Set(vis.Change{Knob: "echo_test_limit", Value: 42, By: vis.Writer{Kind: vis.Human, ID: "sam"}}); err != nil {
		t.Fatal(err)
	}
	if got := get(newServer(in), "/k/a").Body.String(); got != "42" {
		t.Fatalf("body = %q, want 42", got)
	}
}

func TestEchoRoutesAreNamedByTheirPattern(t *testing.T) {
	in := vis.NewInstance()
	e := newServer(in)
	for _, k := range []string{"a", "b", "c"} {
		get(e, "/k/"+k)
	}
	if r := in.Route("GET /k/:key", time.Minute); r.Requests != 3 || r.Errors != 0 {
		t.Fatalf("route = %+v, want 3 requests under the pattern, not the paths", r)
	}
}

func TestEchoErrorsCountWithTheStatusEchoSends(t *testing.T) {
	in := vis.NewInstance()
	e := newServer(in)
	if code := get(e, "/fail").Code; code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 still sent", code)
	}
	if r := in.Route("GET /fail", time.Minute); r.Requests != 1 || r.Errors != 1 {
		t.Fatalf("/fail = %+v, want 1 error", r)
	}
}

func TestEchoPanicsStillReachRecover(t *testing.T) {
	in := vis.NewInstance()
	if code := get(newServer(in), "/boom").Code; code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 from echo's Recover", code)
	}
	if r := in.Route("GET /boom", time.Minute); r.Errors != 1 {
		t.Fatalf("/boom = %+v, want the panic counted", r)
	}
}

func TestEchoUnknownPathsShareOneRoute(t *testing.T) {
	in := vis.NewInstance()
	e := newServer(in)
	get(e, "/nope/1")
	get(e, "/nope/2")
	for _, r := range in.Routes(time.Minute) {
		if r.Route == "GET /nope/1" || r.Route == "GET /nope/2" {
			t.Fatalf("a 404 path became its own route: %+v", in.Routes(time.Minute))
		}
	}
}

func TestEchoMiddlewareUsesTheDefaultInstance(t *testing.T) {
	e := echo.New()
	e.Use(visecho.Middleware())
	e.GET("/d", func(c echo.Context) error { return c.NoContent(http.StatusNoContent) })
	get(e, "/d")
	if r := vis.DefaultInstance().Route("GET /d", time.Minute); r.Requests != 1 {
		t.Fatalf("default instance route = %+v, want 1 request", r)
	}
}
