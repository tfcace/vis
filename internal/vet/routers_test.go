package vet_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Stand-ins for gin and echo, with the methods vet follows, so the test
// needs neither the network nor vis to depend on them.
var fakeGin = `package gin

type Context struct{}
type HandlerFunc func(*Context)
type RouterGroup struct{}
type Engine struct{ RouterGroup }

func New() *Engine     { return &Engine{} }
func Default() *Engine { return &Engine{} }

func (g *RouterGroup) Group(string, ...HandlerFunc) *RouterGroup        { return g }
func (g *RouterGroup) GET(string, ...HandlerFunc)                       {}
func (g *RouterGroup) POST(string, ...HandlerFunc)                      {}
func (g *RouterGroup) DELETE(string, ...HandlerFunc)                    {}
func (g *RouterGroup) Any(string, ...HandlerFunc)                       {}
func (g *RouterGroup) Handle(string, string, ...HandlerFunc)            {}
func (e *Engine) ServeHTTP(any, any)                                    {}
`

var fakeEcho = `package echo

type Context interface{}
type HandlerFunc func(Context) error
type MiddlewareFunc func(HandlerFunc) HandlerFunc
type Echo struct{}
type Group struct{}

func New() *Echo { return &Echo{} }

func (e *Echo) Group(string, ...MiddlewareFunc) *Group           { return &Group{} }
func (e *Echo) GET(string, HandlerFunc, ...MiddlewareFunc)       {}
func (g *Group) PUT(string, HandlerFunc, ...MiddlewareFunc)      {}
func (g *Group) Group(string, ...MiddlewareFunc) *Group          { return g }
`

// vetModule runs a built vis vet in a module of its own that imports the
// stand-ins, and returns its output.
func vetModule(t *testing.T, src string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and vets packages")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "vis")
	if out, err := exec.Command("go", "build", "-C", root, "-o", bin, "./cmd/vis").CombinedOutput(); err != nil {
		t.Fatalf("build vis: %v\n%s", err, out)
	}
	sum, _ := os.ReadFile(filepath.Join(root, "go.sum"))
	files := map[string]string{
		"go.mod": "module app\n\ngo 1.26.0\n\nrequire (\n\tgithub.com/tfcace/vis v0.0.0\n\tgithub.com/gin-gonic/gin v0.0.0\n\tgithub.com/labstack/echo/v4 v4.0.0\n)\n\n" +
			"replace github.com/tfcace/vis => " + root + "\n\nreplace github.com/gin-gonic/gin => ./fakegin\n\nreplace github.com/labstack/echo/v4 => ./fakeecho\n",
		"go.sum":           string(sum),
		"fakegin/go.mod":   "module github.com/gin-gonic/gin\n\ngo 1.26.0\n",
		"fakegin/gin.go":   fakeGin,
		"fakeecho/go.mod":  "module github.com/labstack/echo/v4\n\ngo 1.26.0\n",
		"fakeecho/echo.go": fakeEcho,
		"a/a.go":           src,
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(bin, "vet", "./a")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
	out, _ := cmd.CombinedOutput()
	return string(out)
}

const routerImports = `package a

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labstack/echo/v4"
	"github.com/tfcace/vis"
)

const v2 = "/v2"

func ginRoutes() *gin.Engine {
	r := gin.Default()
	r.POST("/api/generate", generate)
	v1 := r.Group("/v1")
	v1.GET("/models/:model", show)
	r.Group("/api").Group(v2).DELETE("/items/:id", drop)
	r.Any("/proxy/*path", proxy)
	r.Handle("GET", "/handled", show)
	return r
}

func echoRoutes() *echo.Echo {
	e := echo.New()
	e.GET("/k/:key", key)
	g := e.Group("/admin")
	g.PUT("/x", x)
	return e
}

func generate(*gin.Context)       {}
func show(*gin.Context)           {}
func drop(*gin.Context)           {}
func proxy(*gin.Context)          {}
func key(echo.Context) error      { return nil }
func x(echo.Context) error        { return nil }

var _ = time.Second
`

// SLOs naming routes gin and echo register, as visgin and visecho name them,
// are fine; one naming a route nothing registers is reported.
func TestVetChecksSLOsAgainstGinAndEchoRoutes(t *testing.T) {
	good := []string{"POST /api/generate", "GET /v1/models/:model", "DELETE /api/v2/items/:id", "PATCH /proxy/*path", "GET /handled", "GET /k/:key", "PUT /admin/x"}
	src := routerImports
	for _, r := range good {
		src += "var _ = vis.NewSLO(\"" + r + "\", vis.P99(time.Second))\n"
	}
	src += "var _ = vis.NewSLO(\"POST /api/chat\", vis.P99(time.Second))\n"
	src += "var _ = vis.NewSLO(\"GET /v1/model/:model\", vis.P99(time.Second))\n"
	out := vetModule(t, src)
	has(t, out, `no handler in this package is registered under "POST /api/chat"`,
		// with many routes, the nearest ones, not all of them
		`no handler in this package is registered under "GET /v1/model/:model"; the nearest it registers are "GET /v1/models/:model"`)
	if strings.Contains(out, `"TRACE /proxy/*path"`) {
		t.Errorf("vet listed every route instead of the nearest:\n%s", out)
	}
	for _, r := range good {
		if strings.Contains(out, `"`+r+`"`+" (it registers") {
			t.Errorf("vet reported %q, which the package registers:\n%s", r, out)
		}
	}
}

// When a route's path can't be read, vet can't tell which routes exist, so
// it says nothing rather than report an SLO that may be right.
func TestVetIsQuietWhenARouteCantBeRead(t *testing.T) {
	src := routerImports + `
func more(r *gin.Engine, prefix string) {
	r.Group(prefix).GET("/y", show)
}

var _ = vis.NewSLO("GET /somewhere/y", vis.P99(time.Second))
`
	if out := vetModule(t, src); strings.Contains(out, "no handler in this package") {
		t.Fatalf("vet reported an SLO it can't check:\n%s", out)
	}
}
