package visgin_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tfcace/vis"
	"github.com/tfcace/vis/integrations/visgin"
)

var limit = vis.NewInt("gin_test_limit", vis.Range(1, 100), vis.Default(10))

func init() { gin.SetMode(gin.TestMode) }

func newServer(in *vis.Instance) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery()) // outermost, as gin.Default has it
	r.Use(visgin.MiddlewareIn(in))
	r.HandleMethodNotAllowed = true
	r.GET("/k/:key", func(c *gin.Context) { c.String(http.StatusOK, fmt.Sprint(limit.From(c.Request.Context()))) })
	r.GET("/fail", func(c *gin.Context) { c.String(http.StatusBadGateway, "origin down") })
	r.GET("/boom", func(c *gin.Context) { panic("boom") })
	v1 := r.Group("/v1")
	v1.POST("/items/:id", func(c *gin.Context) { c.Status(http.StatusCreated) })
	return r
}

func do(r *gin.Engine, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestGinHandlersReadKnobs(t *testing.T) {
	in := vis.NewInstance()
	if _, err := in.Set(vis.Change{Knob: "gin_test_limit", Value: 42, By: vis.Writer{Kind: vis.Human, ID: "sam"}}); err != nil {
		t.Fatal(err)
	}
	if got := do(newServer(in), "GET", "/k/a").Body.String(); got != "42" {
		t.Fatalf("body = %q, want 42", got)
	}
}

func TestGinRoutesAreNamedByTheirPattern(t *testing.T) {
	in := vis.NewInstance()
	r := newServer(in)
	for _, k := range []string{"a", "b", "c"} {
		do(r, "GET", "/k/"+k)
	}
	if got := in.Route("GET /k/:key", time.Minute); got.Requests != 3 || got.Errors != 0 {
		t.Fatalf("route = %+v, want 3 requests under the pattern, not the paths", got)
	}
}

func TestGinGroupRoutesIncludeTheGroupPrefix(t *testing.T) {
	in := vis.NewInstance()
	do(newServer(in), "POST", "/v1/items/7")
	if got := in.Route("POST /v1/items/:id", time.Minute); got.Requests != 1 {
		t.Fatalf("route = %+v, want the request under the group's full pattern", in.Routes(time.Minute))
	}
}

func TestGinErrorsCountWithTheStatusSent(t *testing.T) {
	in := vis.NewInstance()
	if code := do(newServer(in), "GET", "/fail").Code; code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 still sent", code)
	}
	if got := in.Route("GET /fail", time.Minute); got.Requests != 1 || got.Errors != 1 {
		t.Fatalf("/fail = %+v, want 1 error", got)
	}
}

func TestGinPanicsStillReachRecovery(t *testing.T) {
	in := vis.NewInstance()
	if code := do(newServer(in), "GET", "/boom").Code; code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 from gin's Recovery", code)
	}
	if got := in.Route("GET /boom", time.Minute); got.Errors != 1 {
		t.Fatalf("/boom = %+v, want the panic counted", got)
	}
}

func TestGinUnknownPathsShareOneRoute(t *testing.T) {
	in := vis.NewInstance()
	r := newServer(in)
	do(r, "GET", "/nope/1")
	do(r, "GET", "/nope/2")
	do(r, "DELETE", "/fail") // 405: the path exists, the method doesn't
	for _, got := range in.Routes(time.Minute) {
		switch got.Route {
		case "GET /nope/1", "GET /nope/2", "DELETE /fail", "GET ", "DELETE ":
			t.Fatalf("an unrouted request became its own route %q: %+v", got.Route, in.Routes(time.Minute))
		}
	}
}

func TestGinMiddlewareUsesTheDefaultInstance(t *testing.T) {
	r := gin.New()
	r.Use(visgin.Middleware())
	r.GET("/d", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	do(r, "GET", "/d")
	if got := vis.DefaultInstance().Route("GET /d", time.Minute); got.Requests != 1 {
		t.Fatalf("default instance route = %+v, want 1 request", got)
	}
}
