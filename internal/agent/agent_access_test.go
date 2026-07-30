package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// fakeApp stands in for the deployed application: it answers the authorization
// endpoint however the test needs, and records what it was asked.
type fakeApp struct {
	srv     *httptest.Server
	port    int
	handler func(w http.ResponseWriter, r *http.Request)

	lastPath    string
	lastHeaders http.Header
}

func newFakeApp(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *fakeApp {
	t.Helper()
	app := &fakeApp{handler: handler}
	app.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		app.lastPath = r.URL.Path
		app.lastHeaders = r.Header.Clone()
		app.handler(w, r)
	}))
	t.Cleanup(app.srv.Close)

	u, err := url.Parse(app.srv.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parsing test server port: %v", err)
	}
	app.port = port
	return app
}

// serviceFor builds a Service wired to an app on the given port. A port of 0
// means nothing is live.
func serviceFor(port int) *Service {
	return &Service{
		authMode:       "header",
		authHeader:     "X-Authenticated-User",
		accessMode:     "app",
		accessEndpoint: "/_slot_machine/access",
		livePort:       func() int { return port },
	}
}

func requestAs(user string) *http.Request {
	r := httptest.NewRequest("GET", "/agent/conversations", nil)
	if user != "" {
		r.Header.Set("X-Authenticated-User", user)
	}
	return r
}

func answer(access string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"access":%q}`, access)
	}
}

func TestAuthorizeDelegatesToTheApp(t *testing.T) {
	t.Parallel()

	t.Run("granted", func(t *testing.T) {
		app := newFakeApp(t, answer("granted"))
		v := serviceFor(app.port).authorize(requestAs("alice"), "alice")
		if !v.allow {
			t.Fatalf("granted was refused: %+v", v)
		}
		if v.blanket {
			t.Error("granted is a decision about this user, not a blanket answer")
		}
	})

	t.Run("denied", func(t *testing.T) {
		app := newFakeApp(t, answer("denied"))
		v := serviceFor(app.port).authorize(requestAs("mallory"), "mallory")
		if v.allow {
			t.Fatal("denied was allowed")
		}
		if v.reason != "denied" {
			t.Errorf("reason %q, want denied — a real decision must not be reported "+
				"as an outage", v.reason)
		}
	})

	t.Run("allAuth", func(t *testing.T) {
		app := newFakeApp(t, answer("allAuth"))
		v := serviceFor(app.port).authorize(requestAs("anyone"), "anyone")
		if !v.allow {
			t.Fatalf("allAuth was refused: %+v", v)
		}
		if !v.blanket {
			t.Error("allAuth is a statement about the app; the distinction from " +
				"granted is what lets the UI say no decision was made about this person")
		}
	})

	t.Run("asks the configured endpoint", func(t *testing.T) {
		app := newFakeApp(t, answer("granted"))
		svc := serviceFor(app.port)
		svc.accessEndpoint = "/who-may"
		svc.authorize(requestAs("alice"), "alice")
		if app.lastPath != "/who-may" {
			t.Fatalf("asked %q, want /who-may", app.lastPath)
		}
	})
}

// The app must be able to answer from an SSO group claim rather than its own
// user table, without slot-machine learning what a group is. That only works if
// the proxy's extra identity headers arrive intact.
func TestAuthorizeForwardsIdentityHeaders(t *testing.T) {
	t.Parallel()
	app := newFakeApp(t, answer("granted"))

	r := requestAs("alice@example.com")
	r.Header.Set("X-Auth-Request-Groups", "eng,admin")
	r.Header.Set("X-Auth-Request-Email", "alice@example.com")
	// Credentials that belong to the caller's session and have no business
	// reaching an internal endpoint.
	r.Header.Set("Cookie", "session=super-secret")
	r.Header.Set("Authorization", "Bearer super-secret")

	serviceFor(app.port).authorize(r, "alice@example.com")

	if got := app.lastHeaders.Get("X-Authenticated-User"); got != "alice@example.com" {
		t.Errorf("identity header not forwarded: %q", got)
	}
	if got := app.lastHeaders.Get("X-Auth-Request-Groups"); got != "eng,admin" {
		t.Errorf("group claim not forwarded: %q — an app cannot answer from SSO "+
			"groups if they do not arrive", got)
	}
	if got := app.lastHeaders.Get("Cookie"); got != "" {
		t.Errorf("forwarded the caller's cookie to the app: %q", got)
	}
	if got := app.lastHeaders.Get("Authorization"); got != "" {
		t.Errorf("forwarded the caller's Authorization to the app: %q", got)
	}
}

// Every way of not getting a clean "yes" must end in a refusal. These are listed
// individually because each one is a distinct thing an app can do, and a
// permissive default in any single branch opens the agent to everyone.
func TestAuthorizeFailsClosed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
		reason  string
	}{
		{"endpoint not implemented", func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}, "not-implemented"},

		{"app errors", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", 500)
		}, "app-refused"},

		{"app returns unreadable json", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "not json at all")
		}, "app-unreadable"},

		{"app returns an unknown verdict", func(w http.ResponseWriter, r *http.Request) {
			// A typo in an app's handler must not be a way in.
			fmt.Fprint(w, `{"access":"granted "}`)
		}, "app-unreadable"},

		{"app omits the field", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{}`)
		}, "app-unreadable"},

		{"app redirects to something that would say yes", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/elsewhere" {
				fmt.Fprint(w, `{"access":"granted"}`)
				return
			}
			http.Redirect(w, r, "/elsewhere", 302)
		}, "app-refused"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := newFakeApp(t, tc.handler)
			v := serviceFor(app.port).authorize(requestAs("alice"), "alice")
			if v.allow {
				t.Fatal("allowed without a clean yes from the app")
			}
			if v.reason != tc.reason {
				t.Errorf("reason %q, want %q", v.reason, tc.reason)
			}
		})
	}

	t.Run("app unreachable", func(t *testing.T) {
		// A port nothing is listening on: the app crashed, or is mid-restart.
		v := serviceFor(1).authorize(requestAs("alice"), "alice")
		if v.allow {
			t.Fatal("allowed while the app was unreachable")
		}
		if v.reason != "app-unreachable" {
			t.Errorf("reason %q, want app-unreachable", v.reason)
		}
	})
}

// The decision the user made explicitly: no live app means no chat. This is the
// one refusal that is not about the person asking, and the only one where the
// alternative — granting — would widen access in the least-observed state.
func TestAuthorizeRefusesWithNoLiveApp(t *testing.T) {
	t.Parallel()

	svc := serviceFor(0)
	v := svc.authorize(requestAs("alice"), "alice")
	if v.allow {
		t.Fatal("allowed with no live app; a failed deploy must not open the agent")
	}
	if v.reason != "no-live-app" {
		t.Fatalf("reason %q, want no-live-app", v.reason)
	}

	// A nil livePort is the same situation, not a panic.
	svc.livePort = nil
	if v := svc.authorize(requestAs("alice"), "alice"); v.allow {
		t.Fatal("allowed with no way to find a live app")
	}

	// 503, not 403: nothing has been decided about this user.
	w := httptest.NewRecorder()
	svc.writeAccessDenial(w, accessVerdict{reason: "no-live-app"}, "alice")
	if w.Code != 503 {
		t.Errorf("status %d, want 503 — this is not a judgement about the user", w.Code)
	}
	if !strings.Contains(w.Body.String(), "agent_access") {
		t.Error("the refusal should name the config field that changes it")
	}
}

// The two short-circuits, which must not reach the app at all.
func TestAuthorizeShortCircuits(t *testing.T) {
	t.Parallel()

	asked := false
	app := newFakeApp(t, func(w http.ResponseWriter, r *http.Request) {
		asked = true
		fmt.Fprint(w, `{"access":"denied"}`)
	})

	t.Run("allAuth in config never asks", func(t *testing.T) {
		asked = false
		svc := serviceFor(app.port)
		svc.accessMode = "allAuth"
		if v := svc.authorize(requestAs("alice"), "alice"); !v.allow {
			t.Fatal("allAuth refused")
		}
		if asked {
			t.Error("asked the app despite allAuth; the operator has already answered")
		}
	})

	t.Run("none mode never asks", func(t *testing.T) {
		asked = false
		svc := serviceFor(app.port)
		svc.authMode = "none"
		if v := svc.authorize(requestAs(""), anonymousUser); !v.allow {
			t.Fatal("none mode refused")
		}
		if asked {
			t.Error("asked the app who the empty identity is")
		}
	})
}

// The original defect was structural, not logical: /chat, /chat.css and
// /chat/config were dispatched in a switch above the auth check, and only
// /agent/* was gated. No amount of correctness in extractUser could have helped.
//
// So this asserts the property rather than the implementation — every path the
// handler serves, refused without an identity. A future route added above the
// gate fails here, which is the only way this stays true.
func TestEveryRouteIsGated(t *testing.T) {
	t.Parallel()

	paths := []string{
		"/chat",
		"/chat.css",
		"/chat/config",
		"/agent/conversations",
		"/agent/conversations/conv-1",
		"/agent/conversations/conv-1/messages",
		"/agent/conversations/conv-1/stream",
		"/agent/conversations/conv-1/cancel",
	}

	// An app that would say yes to anyone, to prove the refusal comes from the
	// missing identity and not from the authorization step behind it.
	app := newFakeApp(t, answer("allAuth"))
	svc := serviceFor(app.port)

	for _, path := range paths {
		for _, method := range []string{"GET", "POST", "DELETE"} {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(method, path, strings.NewReader("{}"))
			svc.ServeHTTP(w, r)

			if w.Code != 401 {
				t.Errorf("%s %s: status %d, want 401 — this route is reachable "+
					"without an identity", method, path, w.Code)
			}
		}
	}
}

// The 401 is the first thing an operator sees when their proxy is wrong, and
// they will not have this repository open. It has to be a set of instructions.
func TestUnauthorizedBodyExplainsTheFix(t *testing.T) {
	t.Parallel()

	svc := serviceFor(0)
	svc.authHeader = "X-Remote-User"
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest("GET", "/chat", nil))

	body := w.Body.String()
	for _, want := range []string{
		"X-Remote-User", // the header actually configured, not the default
		"forward_auth",  // something to search for
		"agent_auth",    // the field that turns this off for local work
	} {
		if !strings.Contains(body, want) {
			t.Errorf("401 body does not mention %q:\n%s", want, body)
		}
	}
}

// A 403 is a decision about a person; a 503 is an admission that no decision
// could be made. Conflating them tells an operator to check their own access
// when the real problem is that their app is down.
func TestAccessDenialStatusCodes(t *testing.T) {
	t.Parallel()

	cases := map[string]int{
		"denied":          403,
		"no-live-app":     503,
		"app-unreachable": 503,
		"app-refused":     503,
		"app-unreadable":  503,
		"not-implemented": 503,
	}

	svc := serviceFor(0)
	for reason, want := range cases {
		w := httptest.NewRecorder()
		svc.writeAccessDenial(w, accessVerdict{reason: reason}, "alice")
		if w.Code != want {
			t.Errorf("%s: status %d, want %d", reason, w.Code, want)
		}
	}
}
