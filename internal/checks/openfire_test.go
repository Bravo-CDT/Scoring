package checks

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// fakeOpenfire serves /login.jsp like Openfire's admin console: a GET sets
// the csrf cookie (unless setCookie is false), and a POST carrying the right
// form and cookie gets onSuccess, anything else onFailure.
func fakeOpenfire(t *testing.T, setCookie bool, onSuccess, onFailure http.HandlerFunc) (string, int) {
	t.Helper()
	return newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login.jsp" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			if setCookie {
				http.SetCookie(w, &http.Cookie{Name: "csrf", Value: "tok123", Path: "/"})
			}
			w.Write([]byte("<form>login</form>"))
		case http.MethodPost:
			c, err := r.Cookie("csrf")
			ok := err == nil && c.Value == "tok123" &&
				r.PostFormValue("username") == "admin" &&
				r.PostFormValue("password") == "Patty321!" &&
				r.PostFormValue("csrf") == "tok123" &&
				r.PostFormValue("login") == "true" &&
				r.PostFormValue("url") == "/index.jsp"
			if ok {
				onSuccess(w, r)
			} else {
				onFailure(w, r)
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

// redirectTo replies 302 with Location loc.
func redirectTo(loc string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusFound)
	}
}

// loginPage replies 200 with the login form, as Openfire does on a bad login.
func loginPage(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("<form>login failed</form>"))
}

// validOpenfireParams returns a full, valid set of openfire params for port.
func validOpenfireParams(port int, password string) map[string]any {
	return map[string]any{"port": port, "user": "admin", "password": password}
}

func TestOpenfire_Params(t *testing.T) {
	for _, key := range []string{"port", "user", "password"} {
		t.Run("missing "+key, func(t *testing.T) {
			params := validOpenfireParams(9090, "Patty321!")
			delete(params, key)
			err := ValidateParams("openfire", params)
			if err == nil || !strings.Contains(err.Error(), `"`+key+`"`) {
				t.Fatalf("expected error mentioning %q, got %v", key, err)
			}
		})
	}
	t.Run("unknown param", func(t *testing.T) {
		params := validOpenfireParams(9090, "Patty321!")
		params["bogus"] = 1
		err := ValidateParams("openfire", params)
		if err == nil || !strings.Contains(err.Error(), `"bogus"`) {
			t.Fatalf("expected error mentioning \"bogus\", got %v", err)
		}
	})
	if err := ValidateParams("openfire", validOpenfireParams(9090, "Patty321!")); err != nil {
		t.Errorf("valid params rejected: %v", err)
	}
}

func TestOpenfire_LoginSucceeds(t *testing.T) {
	host, port := fakeOpenfire(t, true, redirectTo("index.jsp"), loginPage)
	res := Openfire(context.Background(), host, validOpenfireParams(port, "Patty321!"))
	if !res.Success {
		t.Errorf("expected success, got Detail=%q", res.Detail)
	}
}

func TestOpenfire_LoginRejected(t *testing.T) {
	host, port := fakeOpenfire(t, true, redirectTo("index.jsp"), loginPage)
	res := Openfire(context.Background(), host, validOpenfireParams(port, "wrong-secret"))
	if res.Success {
		t.Fatalf("expected failure, got %+v", res)
	}
	if !strings.Contains(res.Detail, "status 200") {
		t.Errorf("Detail %q should contain \"status 200\"", res.Detail)
	}
	if strings.Contains(res.Detail, "wrong-secret") {
		t.Errorf("Detail %q must not contain the password", res.Detail)
	}
}

func TestOpenfire_RedirectBackToLogin(t *testing.T) {
	failed := redirectTo("login.jsp?login=failed")
	host, port := fakeOpenfire(t, true, failed, failed)
	res := Openfire(context.Background(), host, validOpenfireParams(port, "Patty321!"))
	if res.Success {
		t.Fatalf("expected failure, got %+v", res)
	}
	if !strings.Contains(res.Detail, "login.jsp") {
		t.Errorf("Detail %q should say it was redirected back to login.jsp", res.Detail)
	}
}

func TestOpenfire_NoCSRFCookie(t *testing.T) {
	host, port := fakeOpenfire(t, false, redirectTo("index.jsp"), loginPage)
	res := Openfire(context.Background(), host, validOpenfireParams(port, "Patty321!"))
	if res.Success {
		t.Fatalf("expected failure, got %+v", res)
	}
	if !strings.Contains(res.Detail, "csrf") {
		t.Errorf("Detail %q should mention \"csrf\"", res.Detail)
	}
}

func TestOpenfire_RedirectWithoutLocation(t *testing.T) {
	host, port := fakeOpenfire(t, true, redirectTo(""), loginPage)
	res := Openfire(context.Background(), host, validOpenfireParams(port, "Patty321!"))
	if res.Success {
		t.Fatalf("expected failure, got %+v", res)
	}
}
