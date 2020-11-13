package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	gosundheit "github.com/AppsFlyer/go-sundheit"
	"github.com/AppsFlyer/go-sundheit/checks"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/dexidp/dex/storage"
)

func TestHandleHealth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	httpServer, server := newTestServer(ctx, t, nil)
	defer httpServer.Close()

	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 got %d", rr.Code)
	}
}

func TestHandleHealthFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	httpServer, server := newTestServer(ctx, t, func(c *Config) {
		c.HealthChecker = gosundheit.New()

		c.HealthChecker.RegisterCheck(&gosundheit.Config{
			Check: &checks.CustomCheck{
				CheckName: "fail",
				CheckFunc: func() (details interface{}, err error) {
					return nil, errors.New("error")
				},
			},
			InitiallyPassing: false,
			ExecutionPeriod:  1 * time.Second,
		})
	})
	defer httpServer.Close()

	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 got %d", rr.Code)
	}
}

type emptyStorage struct {
	storage.Storage
}

func (*emptyStorage) GetAuthRequest(string) (storage.AuthRequest, error) {
	return storage.AuthRequest{}, storage.ErrNotFound
}

func TestHandleInvalidOAuth2Callbacks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	httpServer, server := newTestServer(ctx, t, func(c *Config) {
		c.Storage = &emptyStorage{c.Storage}
	})
	defer httpServer.Close()

	tests := []struct {
		TargetURI    string
		ExpectedCode int
	}{
		{"/callback", http.StatusBadRequest},
		{"/callback?code=&state=", http.StatusBadRequest},
		{"/callback?code=AAAAAAA&state=BBBBBBB", http.StatusBadRequest},
	}

	rr := httptest.NewRecorder()

	for i, r := range tests {
		server.ServeHTTP(rr, httptest.NewRequest("GET", r.TargetURI, nil))
		if rr.Code != r.ExpectedCode {
			t.Fatalf("test %d expected %d, got %d", i, r.ExpectedCode, rr.Code)
		}
	}
}

func TestHandleInvalidSAMLCallbacks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	httpServer, server := newTestServer(ctx, t, func(c *Config) {
		c.Storage = &emptyStorage{c.Storage}
	})
	defer httpServer.Close()

	type requestForm struct {
		RelayState string
	}
	tests := []struct {
		RequestForm  requestForm
		ExpectedCode int
	}{
		{requestForm{}, http.StatusBadRequest},
		{requestForm{RelayState: "AAAAAAA"}, http.StatusBadRequest},
	}

	rr := httptest.NewRecorder()

	for i, r := range tests {
		jsonValue, err := json.Marshal(r.RequestForm)
		if err != nil {
			t.Fatal(err.Error())
		}
		server.ServeHTTP(rr, httptest.NewRequest("POST", "/callback", bytes.NewBuffer(jsonValue)))
		if rr.Code != r.ExpectedCode {
			t.Fatalf("test %d expected %d, got %d", i, r.ExpectedCode, rr.Code)
		}
	}
}

// TestHandleAuthCode checks that it is forbidden to use same code twice
func TestHandleAuthCode(t *testing.T) {
	tests := []struct {
		name       string
		handleCode func(*testing.T, context.Context, *oauth2.Config, string)
	}{
		{
			name: "Code Reuse should return invalid_grant",
			handleCode: func(t *testing.T, ctx context.Context, oauth2Config *oauth2.Config, code string) {
				_, err := oauth2Config.Exchange(ctx, code)
				require.NoError(t, err)

				_, err = oauth2Config.Exchange(ctx, code)
				require.Error(t, err)

				oauth2Err, ok := err.(*oauth2.RetrieveError)
				require.True(t, ok)

				var errResponse struct{ Error string }
				err = json.Unmarshal(oauth2Err.Body, &errResponse)
				require.NoError(t, err)

				// invalid_grant must be returned for invalid values
				// https://tools.ietf.org/html/rfc6749#section-5.2
				require.Equal(t, errInvalidGrant, errResponse.Error)
			},
		},
		{
			name: "No Code should return invalid_request",
			handleCode: func(t *testing.T, ctx context.Context, oauth2Config *oauth2.Config, _ string) {
				_, err := oauth2Config.Exchange(ctx, "")
				require.Error(t, err)

				oauth2Err, ok := err.(*oauth2.RetrieveError)
				require.True(t, ok)

				var errResponse struct{ Error string }
				err = json.Unmarshal(oauth2Err.Body, &errResponse)
				require.NoError(t, err)

				require.Equal(t, errInvalidRequest, errResponse.Error)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			httpServer, s := newTestServer(ctx, t, func(c *Config) { c.Issuer += "/non-root-path" })
			defer httpServer.Close()

			p, err := oidc.NewProvider(ctx, httpServer.URL)
			require.NoError(t, err)

			var oauth2Client oauth2Client
			oauth2Client.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/callback" {
					http.Redirect(w, r, oauth2Client.config.AuthCodeURL(""), http.StatusSeeOther)
					return
				}

				q := r.URL.Query()
				require.Equal(t, q.Get("error"), "", q.Get("error_description"))

				code := q.Get("code")
				tc.handleCode(t, ctx, oauth2Client.config, code)

				w.WriteHeader(http.StatusOK)
			}))
			defer oauth2Client.server.Close()

			redirectURL := oauth2Client.server.URL + "/callback"
			client := storage.Client{
				ID:           "testclient",
				Secret:       "testclientsecret",
				RedirectURIs: []string{redirectURL},
			}
			err = s.storage.CreateClient(client)
			require.NoError(t, err)

			oauth2Client.config = &oauth2.Config{
				ClientID:     client.ID,
				ClientSecret: client.Secret,
				Endpoint:     p.Endpoint(),
				Scopes:       []string{oidc.ScopeOpenID, "email", "offline_access"},
				RedirectURL:  redirectURL,
			}

			resp, err := http.Get(oauth2Client.server.URL + "/login")
			require.NoError(t, err)

			resp.Body.Close()
		})
	}
}
