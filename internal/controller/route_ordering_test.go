package controller

import (
	"net/http"
	"os"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// freshTestApp builds a tests.TestApp against an empty data dir so our
// migrations can apply cleanly. PB's stock tests.NewTestApp() clones the
// PB test stub data which has users without `code` values — that violates
// the unique idx_users_code our init migration adds. An empty temp dir
// avoids the conflict.
func freshTestApp(t testing.TB) *tests.TestApp {
	t.Setenv("KIOSK_QUIET_BOOTSTRAP", "1")
	app, err := tests.NewTestApp(t.TempDir())
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	return app
}

// TestRouteOrdering_SpecificWinsOverCatchAll pins the framework-level
// invariant that motivated the cmd/controller/main.go reorder: when a
// specific /api/kiosk/* route is registered alongside the catch-all SPA
// /{path...} route, the specific route wins regardless of registration
// order. If PB ever changes this matching behavior, the integrity /
// reconcile / health endpoints would all silently start serving SPA HTML
// (or whatever apis.Static decides to return) instead of their handlers.
//
// The test deliberately uses a stand-in /api/kiosk/test-marker path rather
// than the real catalog integrity endpoint, because the real handler
// depends on a NATS-backed CatalogPublisher we don't want to spin up in a
// route-routing test. The registration pattern is byte-identical to
// cmd/controller/main.go: specific routes first, catch-all last.
func TestRouteOrdering_SpecificWinsOverCatchAll(t *testing.T) {
	staticDir := t.TempDir() // empty — apis.Static would 404 / serve nothing useful

	scenario := tests.ApiScenario{
		TestAppFactory: freshTestApp,
		Name:           "specific route wins over SPA catch-all",
		Method:         http.MethodGet,
		URL:            "/api/kiosk/test-marker",
		BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
			e.Router.GET("/api/kiosk/test-marker", func(re *core.RequestEvent) error {
				return re.JSON(http.StatusOK, map[string]string{
					"route": "specific",
				})
			})
			e.Router.GET("/{path...}", apis.Static(os.DirFS(staticDir), true))
		},
		ExpectedStatus: http.StatusOK,
		ExpectedContent: []string{
			`"route":"specific"`,
		},
	}
	scenario.Test(t)
}

// TestRouteOrdering_CatchAllServesUnknown is the symmetric case: an
// unmatched path falls through to the catch-all. Combined with the test
// above, this proves the routing model is "specific first, fallback to
// catch-all" — which is what makes the cmd/controller/main.go ordering
// safe.
func TestRouteOrdering_CatchAllServesUnknown(t *testing.T) {
	staticDir := t.TempDir()

	scenario := tests.ApiScenario{
		TestAppFactory: freshTestApp,
		Name:           "catch-all serves unknown path",
		Method:         http.MethodGet,
		URL:            "/some/random/path",
		BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
			e.Router.GET("/api/kiosk/test-marker", func(re *core.RequestEvent) error {
				return re.JSON(http.StatusOK, map[string]string{"route": "specific"})
			})
			e.Router.GET("/{path...}", apis.Static(os.DirFS(staticDir), true))
		},
		// Static with empty dir + indexFallback=true → 404 (no index.html).
		// We only care that the request reached the catch-all (it didn't
		// 404 from the router itself); the file-not-found message confirms
		// apis.Static handled the request, not a router-level miss.
		ExpectedStatus: http.StatusNotFound,
		ExpectedContent: []string{
			`"message":"File not found.`,
		},
	}
	scenario.Test(t)
}
