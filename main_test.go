package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	altshiftContext "github.com/altshiftab/utils_go/pkg/context"
	altshiftEnvErrors "github.com/altshiftab/utils_go/pkg/env/errors"
	altshiftErrors "github.com/altshiftab/utils_go/pkg/errors"
	"github.com/altshiftab/utils_go/pkg/errors/types/empty_error"
	altshiftHttpErrors "github.com/altshiftab/utils_go/pkg/http/errors"
	oauth2Errors "github.com/altshiftab/utils_go/pkg/oauth2/errors"
	"github.com/altshiftab/utils_go/pkg/oauth2/types/config"
	"github.com/altshiftab/utils_go/pkg/oauth2/types/endpoint"
	"github.com/altshiftab/utils_go/pkg/oauth2/types/token"
)

const (
	testClientId     = "client-id.apps.googleusercontent.com"
	testClientSecret = "client-secret-value"
	testRefreshToken = "refresh-token-value"
	testAccessToken  = "access-token-value"
	testUserId       = "user@example.com"
	testTopicName    = "projects/project/topics/gmail"
	testHistoryId    = "123456"
	testExpiration   = "1763481600000"

	watchFailedMessage = "An error occurred when performing the watch. Exiting."
)

// testSecrets are the values no log entry may hold.
var testSecrets = []string{testClientSecret, testRefreshToken, testAccessToken}

// fakeGoogle stands in for Google's token endpoint and the Gmail API, each on a server of its own. As the real ones
// do, the token endpoint issues an access token only to the client's own credentials and refresh token, and Gmail
// registers a watch only for a request bearing an access token the endpoint issued, and only on a named topic.
type fakeGoogle struct {
	// dropTokenConnection has the token endpoint close the connection without answering.
	dropTokenConnection bool
	// answerWatch, when set, answers a watch request that would otherwise have been registered.
	answerWatch func(w http.ResponseWriter)

	mutex         sync.Mutex
	tokenRequests []url.Values
	watchRequests []map[string]any

	tokenUrl     *url.URL
	gmailBaseUrl *url.URL
}

func writeJson(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.MarshalWrite(w, value); err != nil {
		t.Errorf("json marshal write: %v", err)
	}
}

func (f *fakeGoogle) serveToken(t *testing.T, w http.ResponseWriter, r *http.Request) {
	if f.dropTokenConnection {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("the response writer cannot be hijacked")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		if err := conn.Close(); err != nil {
			t.Errorf("conn close: %v", err)
		}
		return
	}

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		writeJson(t, w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJson(t, w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	form := r.PostForm

	f.mutex.Lock()
	f.tokenRequests = append(f.tokenRequests, form)
	f.mutex.Unlock()

	if form.Get("client_id") != testClientId || form.Get("client_secret") != testClientSecret {
		writeJson(t, w, http.StatusUnauthorized, map[string]string{
			"error":             "invalid_client",
			"error_description": "The OAuth client was not found.",
		})
		return
	}
	if form.Get("grant_type") != "refresh_token" {
		writeJson(t, w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	if form.Get("refresh_token") != testRefreshToken {
		writeJson(t, w, http.StatusBadRequest, map[string]string{
			"error":             "invalid_grant",
			"error_description": "Token has been expired or revoked.",
		})
		return
	}

	writeJson(t, w, http.StatusOK, map[string]any{
		"access_token": testAccessToken,
		"expires_in":   3599,
		"scope":        "https://www.googleapis.com/auth/gmail.readonly",
		"token_type":   "Bearer",
	})
}

func googleError(code int, message string, status string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": message, "status": status}}
}

func (f *fakeGoogle) serveWatch(t *testing.T, w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+testAccessToken {
		writeJson(
			t, w, http.StatusUnauthorized,
			googleError(http.StatusUnauthorized, "Request had invalid authentication credentials.", "UNAUTHENTICATED"),
		)
		return
	}

	var body map[string]any
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		writeJson(t, w, http.StatusBadRequest, googleError(http.StatusBadRequest, "Invalid JSON payload.", "INVALID_ARGUMENT"))
		return
	}

	f.mutex.Lock()
	f.watchRequests = append(f.watchRequests, body)
	f.mutex.Unlock()

	if topicName, _ := body["topicName"].(string); topicName == "" {
		writeJson(t, w, http.StatusBadRequest, googleError(http.StatusBadRequest, "Invalid topicName", "INVALID_ARGUMENT"))
		return
	}

	if f.answerWatch != nil {
		f.answerWatch(w)
		return
	}

	writeJson(t, w, http.StatusOK, map[string]string{"historyId": testHistoryId, "expiration": testExpiration})
}

func (f *fakeGoogle) start(t *testing.T) {
	t.Helper()

	tokenMux := http.NewServeMux()
	tokenMux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) { f.serveToken(t, w, r) })
	tokenServer := httptest.NewServer(tokenMux)
	t.Cleanup(tokenServer.Close)

	gmailMux := http.NewServeMux()
	gmailMux.HandleFunc(
		"POST /gmail/v1/users/"+url.PathEscape(testUserId)+"/watch",
		func(w http.ResponseWriter, r *http.Request) { f.serveWatch(t, w, r) },
	)
	gmailServer := httptest.NewServer(gmailMux)
	t.Cleanup(gmailServer.Close)

	tokenUrl, err := url.Parse(tokenServer.URL)
	if err != nil {
		t.Fatalf("url parse (token server): %v", err)
	}
	tokenUrl.Path = "/token"
	f.tokenUrl = tokenUrl

	gmailBaseUrl, err := url.Parse(gmailServer.URL)
	if err != nil {
		t.Fatalf("url parse (gmail server): %v", err)
	}
	f.gmailBaseUrl = gmailBaseUrl
}

func (f *fakeGoogle) recorded() ([]url.Values, []map[string]any) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return slices.Clone(f.tokenRequests), slices.Clone(f.watchRequests)
}

func validSettings() *settings {
	return &settings{
		TopicName:         testTopicName,
		ClientId:          testClientId,
		ClientSecret:      testClientSecret,
		RefreshToken:      testRefreshToken,
		UserId:            testUserId,
		LabelIds:          []string{"INBOX", "SENT"},
		LabelFilterAction: "include",
	}
}

// logEntries reads the JSON lines a logger wrote.
func logEntries(t *testing.T, data []byte) []map[string]any {
	t.Helper()

	var entries []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("json unmarshal (log line %q): %v", scanner.Text(), err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}
	return entries
}

// logFailure logs an error as main's FatalWithExitingMessage does, without the exit.
func logFailure(logger *slog.Logger, message string, err error) {
	logger.ErrorContext(altshiftContext.WithError(context.Background(), altshiftErrors.New(err)), message)
}

func TestRun(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name                string
		modifySettings      func(s *settings)
		dropTokenConnection bool
		answerWatch         func(w http.ResponseWriter)

		wantErr           bool
		wantRetrieveError string
		wantStatusCode    int
		wantTokenRequests int
		wantWatchBodies   []map[string]any
	}{
		{
			name:              "registers the watch",
			wantTokenRequests: 1,
			wantWatchBodies: []map[string]any{
				{"topicName": testTopicName, "labelIds": []any{"INBOX", "SENT"}, "labelFilterAction": "include"},
			},
		},
		{
			name: "registers a watch on the whole mailbox without label ids",
			modifySettings: func(s *settings) {
				s.LabelIds = nil
				s.LabelFilterAction = "exclude"
			},
			wantTokenRequests: 1,
			wantWatchBodies:   []map[string]any{{"topicName": testTopicName, "labelFilterAction": "exclude"}},
		},
		{
			name:              "revoked refresh token",
			modifySettings:    func(s *settings) { s.RefreshToken = "revoked-refresh-token" },
			wantErr:           true,
			wantRetrieveError: "invalid_grant",
			wantTokenRequests: 1,
		},
		{
			name:              "wrong client secret",
			modifySettings:    func(s *settings) { s.ClientSecret = "wrong-client-secret" },
			wantErr:           true,
			wantRetrieveError: "invalid_client",
			wantTokenRequests: 1,
		},
		{
			name:                "token endpoint drops the connection",
			dropTokenConnection: true,
			wantErr:             true,
		},
		{
			name: "watch refused",
			answerWatch: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":{"code":403,"message":"User not authorized to perform this action.","status":"PERMISSION_DENIED"}}`)
			},
			wantErr:           true,
			wantStatusCode:    http.StatusForbidden,
			wantTokenRequests: 1,
			wantWatchBodies: []map[string]any{
				{"topicName": testTopicName, "labelIds": []any{"INBOX", "SENT"}, "labelFilterAction": "include"},
			},
		},
		{
			name:              "empty watch response",
			answerWatch:       func(w http.ResponseWriter) { w.WriteHeader(http.StatusOK) },
			wantErr:           true,
			wantTokenRequests: 1,
			wantWatchBodies: []map[string]any{
				{"topicName": testTopicName, "labelIds": []any{"INBOX", "SENT"}, "labelFilterAction": "include"},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeGoogle{dropTokenConnection: testCase.dropTokenConnection, answerWatch: testCase.answerWatch}
			fake.start(t)

			s := validSettings()
			if testCase.modifySettings != nil {
				testCase.modifySettings(s)
			}

			var logBuffer bytes.Buffer
			logger := newLogger(&logBuffer, fake.tokenUrl)

			err := run(context.Background(), logger.Logger, s, fake.tokenUrl, fake.gmailBaseUrl)
			if err != nil {
				logFailure(logger.Logger, watchFailedMessage, err)
			}

			if (err != nil) != testCase.wantErr {
				t.Fatalf("run error = %v, want error: %t", err, testCase.wantErr)
			}

			if testCase.wantRetrieveError != "" {
				retrieveErr, ok := errors.AsType[*oauth2Errors.RetrieveError](err)
				if !ok {
					t.Fatalf("run error = %v, want a token retrieval error", err)
				}
				if retrieveErr.ErrorCode != testCase.wantRetrieveError {
					t.Errorf("token error code = %q, want %q", retrieveErr.ErrorCode, testCase.wantRetrieveError)
				}
			}

			if testCase.wantStatusCode != 0 {
				statusErr, ok := errors.AsType[*altshiftHttpErrors.Non2xxStatusCodeError](err)
				if !ok {
					t.Fatalf("run error = %v, want a status code error", err)
				}
				if statusErr.StatusCode != testCase.wantStatusCode {
					t.Errorf("status code = %d, want %d", statusErr.StatusCode, testCase.wantStatusCode)
				}
			}

			tokenRequests, watchBodies := fake.recorded()

			if len(tokenRequests) != testCase.wantTokenRequests {
				t.Fatalf("token requests = %d, want %d", len(tokenRequests), testCase.wantTokenRequests)
			}
			for _, form := range tokenRequests {
				want := url.Values{
					"grant_type":    {"refresh_token"},
					"refresh_token": {s.RefreshToken},
					"client_id":     {s.ClientId},
					"client_secret": {s.ClientSecret},
				}
				if !reflect.DeepEqual(form, want) {
					t.Errorf("token request form = %v, want %v", form, want)
				}
			}

			if !reflect.DeepEqual(watchBodies, testCase.wantWatchBodies) {
				t.Errorf("watch request bodies = %v, want %v", watchBodies, testCase.wantWatchBodies)
			}

			logged := logBuffer.String()
			for _, secret := range append(slices.Clone(testSecrets), s.ClientSecret, s.RefreshToken) {
				if strings.Contains(logged, secret) {
					t.Errorf("the log holds the secret %q:\n%s", secret, logged)
				}
			}

			entries := logEntries(t, logBuffer.Bytes())
			if len(entries) != 1 {
				t.Fatalf("log entries = %d, want 1:\n%s", len(entries), logged)
			}
			entry := entries[0]

			if testCase.wantErr {
				if entry["message"] != watchFailedMessage || entry["severity"] != "ERROR" {
					t.Errorf("log entry = %v, want an error entry %q", entry, watchFailedMessage)
				}
				return
			}

			wantFields := map[string]any{
				"message":    "Watch registered.",
				"severity":   "INFO",
				"topicName":  testTopicName,
				"userId":     testUserId,
				"historyId":  testHistoryId,
				"expiration": testExpiration,
			}
			for key, want := range wantFields {
				if entry[key] != want {
					t.Errorf("log entry %s = %v, want %v", key, entry[key], want)
				}
			}
		})
	}
}

// lookup is the value at a path of keys into a log entry, or nil where there is none.
func lookup(entry map[string]any, keys ...string) any {
	var value any = entry
	for _, key := range keys {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = object[key]
	}
	return value
}

func TestNewLogger(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name                string
		dropTokenConnection bool
		answerWatch         func(w http.ResponseWriter)
		// fail makes the error to log against the fake.
		fail func(fake *fakeGoogle) error

		wantPath        string
		wantBodyContent string
	}{
		{
			name:                "the body of a token request is masked",
			dropTokenConnection: true,
			fail: func(fake *fakeGoogle) error {
				oauth2Config := &config.Config{
					ClientID:     testClientId,
					ClientSecret: testClientSecret,
					Endpoint:     endpoint.Endpoint{TokenURL: fake.tokenUrl.String(), AuthStyle: endpoint.AuthStyleInParams},
				}
				_, err := oauth2Config.TokenSource(context.Background(), &token.Token{RefreshToken: testRefreshToken}).Token()
				return err
			},
			wantPath:        "/token",
			wantBodyContent: "(MASKED)",
		},
		{
			name:        "the body of a watch request is kept",
			answerWatch: func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) },
			fail: func(fake *fakeGoogle) error {
				return run(context.Background(), slog.New(slog.DiscardHandler), validSettings(), fake.tokenUrl, fake.gmailBaseUrl)
			},
			wantPath:        "/gmail/v1/users/" + testUserId + "/watch",
			wantBodyContent: `{"topicName":"` + testTopicName + `","labelIds":["INBOX","SENT"],"labelFilterAction":"include"}`,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeGoogle{dropTokenConnection: testCase.dropTokenConnection, answerWatch: testCase.answerWatch}
			fake.start(t)

			err := testCase.fail(fake)
			if err == nil {
				t.Fatal("no error to log")
			}

			var logBuffer bytes.Buffer
			logFailure(newLogger(&logBuffer, fake.tokenUrl).Logger, watchFailedMessage, err)
			logged := logBuffer.String()

			for _, secret := range testSecrets {
				if strings.Contains(logged, secret) {
					t.Errorf("the log holds the secret %q:\n%s", secret, logged)
				}
			}

			entries := logEntries(t, logBuffer.Bytes())
			if len(entries) != 1 {
				t.Fatalf("log entries = %d, want 1:\n%s", len(entries), logged)
			}
			entry := entries[0]

			if path := lookup(entry, "error", "context", "url", "path"); path != testCase.wantPath {
				t.Errorf("logged request path = %v, want %q", path, testCase.wantPath)
			}
			content := lookup(entry, "error", "context", "http", "request", "body", "content")
			if content != testCase.wantBodyContent {
				t.Errorf("logged request body = %v, want %q", content, testCase.wantBodyContent)
			}
		})
	}
}

var settingsVariables = []string{
	"TOPIC_NAME", "CLIENT_ID", "CLIENT_SECRET", "REFRESH_TOKEN", "USER_ID", "LABEL_IDS", "LABEL_FILTER_ACTION",
}

func TestReadSettings(t *testing.T) {
	required := map[string]string{
		"TOPIC_NAME":    testTopicName,
		"CLIENT_ID":     testClientId,
		"CLIENT_SECRET": testClientSecret,
		"REFRESH_TOKEN": testRefreshToken,
		"USER_ID":       testUserId,
	}
	with := func(overrides map[string]string, unset ...string) map[string]string {
		environment := maps.Clone(required)
		maps.Copy(environment, overrides)
		for _, name := range unset {
			delete(environment, name)
		}
		return environment
	}

	testCases := []struct {
		name        string
		environment map[string]string

		wantErr               error
		wantEmptyError        bool
		wantErrNaming         string
		wantLabelIds          []string
		wantLabelFilterAction string
	}{
		{
			name:                  "defaults",
			environment:           with(nil),
			wantLabelIds:          []string{"INBOX", "SENT"},
			wantLabelFilterAction: "include",
		},
		{
			name:                  "empty label ids are the default",
			environment:           with(map[string]string{"LABEL_IDS": ""}),
			wantLabelIds:          []string{"INBOX", "SENT"},
			wantLabelFilterAction: "include",
		},
		{
			name:                  "label ids are trimmed and empty ones left out",
			environment:           with(map[string]string{"LABEL_IDS": " INBOX , ,Label_7 ", "LABEL_FILTER_ACTION": "exclude"}),
			wantLabelIds:          []string{"INBOX", "Label_7"},
			wantLabelFilterAction: "exclude",
		},
		{
			name:                  "separators alone are no label ids",
			environment:           with(map[string]string{"LABEL_IDS": " , "}),
			wantLabelFilterAction: "include",
		},
		{
			name:          "missing topic name",
			environment:   with(nil, "TOPIC_NAME"),
			wantErr:       altshiftEnvErrors.ErrNotPresent,
			wantErrNaming: "TOPIC_NAME",
		},
		{
			name:          "missing user id",
			environment:   with(nil, "USER_ID"),
			wantErr:       altshiftEnvErrors.ErrNotPresent,
			wantErrNaming: "USER_ID",
		},
		{
			name:           "empty client secret",
			environment:    with(map[string]string{"CLIENT_SECRET": ""}),
			wantEmptyError: true,
			wantErrNaming:  "CLIENT_SECRET",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			for _, name := range settingsVariables {
				t.Setenv(name, "")
				if value, ok := testCase.environment[name]; ok {
					t.Setenv(name, value)
				} else if err := os.Unsetenv(name); err != nil {
					t.Fatalf("os unsetenv: %v", err)
				}
			}

			s, err := readSettings()

			if testCase.wantErrNaming != "" {
				if err == nil {
					t.Fatalf("read settings error = nil, want one naming %s", testCase.wantErrNaming)
				}
				if testCase.wantErr != nil && !errors.Is(err, testCase.wantErr) {
					t.Errorf("read settings error = %v, want %v", err, testCase.wantErr)
				}
				if _, ok := errors.AsType[*empty_error.Error](err); ok != testCase.wantEmptyError {
					t.Errorf("read settings error = %v, empty error: %t, want %t", err, ok, testCase.wantEmptyError)
				}
				if !strings.Contains(err.Error(), testCase.wantErrNaming) {
					t.Errorf("read settings error = %v, want it naming %s", err, testCase.wantErrNaming)
				}
				return
			}

			if err != nil {
				t.Fatalf("read settings: %v", err)
			}
			want := &settings{
				TopicName:         testTopicName,
				ClientId:          testClientId,
				ClientSecret:      testClientSecret,
				RefreshToken:      testRefreshToken,
				UserId:            testUserId,
				LabelIds:          testCase.wantLabelIds,
				LabelFilterAction: testCase.wantLabelFilterAction,
			}
			if !reflect.DeepEqual(s, want) {
				t.Errorf("settings = %+v, want %+v", s, want)
			}
		})
	}
}
