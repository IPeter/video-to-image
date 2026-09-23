package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	"video-to-image/internal/analyzer"
)

// recordedRequest is everything a request carried, for leak checks.
type recordedRequest struct {
	Method, Path, Query string
	Header              http.Header
	Body                []byte
}

// fakeGemini speaks the subset of the Gemini REST API the SDK uses here: the resumable
// Files upload protocol, files get/list/delete and models:generateContent.
type fakeGemini struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	requests  []recordedRequest
	files     map[string]*genai.File // id → file
	gets      map[string]int         // id → Get count
	nextID    int
	createErr []int // status codes for successive upload "start" requests
	// genStatus holds status codes for successive generateContent calls; 200 or an
	// exhausted list answers genBody.
	genStatus []int
	genBody   string
	genBodies [][]byte // request bodies of generateContent
	// processingGets: how many Gets report PROCESSING before a file turns ACTIVE.
	processingGets int
}

func newFakeGemini(t *testing.T) *fakeGemini {
	f := &fakeGemini{t: t, files: map[string]*genai.File{}, gets: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGemini) client(t *testing.T, mod ...func(*Options)) *Client {
	t.Helper()
	opts := Options{
		APIKey: "test-key", BaseURL: f.srv.URL + "/", Logger: discardLogger(),
		BackoffBase: time.Millisecond, MaxBackoff: 20 * time.Millisecond,
		PollInterval: time.Millisecond, FileActiveTimeout: 5 * time.Second,
	}
	for _, m := range mod {
		m(&opts)
	}
	c, err := New(context.Background(), opts)
	require.NoError(t, err)
	return c
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiErrorBody(code int, status, msg string, details ...map[string]any) map[string]any {
	e := map[string]any{"code": code, "status": status, "message": msg}
	if len(details) > 0 {
		e["details"] = details
	}
	return map[string]any{"error": e}
}

func (f *fakeGemini) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body})
	if r.Header.Get("x-goog-api-key") != "test-key" {
		writeJSON(w, 401, apiErrorBody(401, "UNAUTHENTICATED", "missing key"))
		return
	}

	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/upload/v1beta/files":
		if len(f.createErr) > 0 {
			code := f.createErr[0]
			f.createErr = f.createErr[1:]
			writeJSON(w, code, apiErrorBody(code, "RESOURCE_EXHAUSTED", "upload quota"))
			return
		}
		// Handlers run on server goroutines: assert (not require) and bail out.
		var req struct {
			File genai.File `json:"file"`
		}
		if !assert.Equal(f.t, "resumable", r.Header.Get("X-Goog-Upload-Protocol")) ||
			!assert.Equal(f.t, "start", r.Header.Get("X-Goog-Upload-Command")) ||
			!assert.NoError(f.t, json.Unmarshal(body, &req)) {
			http.Error(w, "bad upload start", http.StatusBadRequest)
			return
		}
		f.nextID++
		id := fmt.Sprintf("vid%d", f.nextID)
		f.files[id] = &genai.File{
			Name: "files/" + id, DisplayName: req.File.DisplayName,
			MIMEType: r.Header.Get("X-Goog-Upload-Header-Content-Type"),
			URI:      f.srv.URL + "/v1beta/files/" + id, State: genai.FileStateProcessing,
			CreateTime: time.Now().UTC(),
		}
		w.Header().Set("X-Goog-Upload-URL", f.srv.URL+"/upload-session/"+id)
		w.WriteHeader(http.StatusOK)

	case r.Method == http.MethodPost && strings.HasPrefix(path, "/upload-session/"):
		id := strings.TrimPrefix(path, "/upload-session/")
		file, ok := f.files[id]
		if !assert.True(f.t, ok, "unknown upload session %s", id) ||
			!assert.Contains(f.t, r.Header.Get("X-Goog-Upload-Command"), "finalize") {
			http.Error(w, "bad upload chunk", http.StatusBadRequest)
			return
		}
		file.SizeBytes = genai.Ptr(int64(len(body)))
		w.Header().Set("X-Goog-Upload-Status", "final")
		writeJSON(w, 200, map[string]any{"file": file})

	case r.Method == http.MethodGet && path == "/v1beta/files":
		// Two pages, to exercise pagination.
		var all []*genai.File
		for i := 1; i <= f.nextID; i++ {
			if file, ok := f.files[fmt.Sprintf("vid%d", i)]; ok {
				all = append(all, file)
			}
		}
		q, _ := url.ParseQuery(r.URL.RawQuery)
		if q.Get("pageToken") == "" && len(all) > 1 {
			writeJSON(w, 200, map[string]any{"files": all[:1], "nextPageToken": "page2"})
			return
		}
		if q.Get("pageToken") == "page2" {
			all = all[1:]
		}
		writeJSON(w, 200, map[string]any{"files": all})

	case strings.HasPrefix(path, "/v1beta/files/"):
		id := strings.TrimPrefix(path, "/v1beta/files/")
		file, ok := f.files[id]
		if !ok {
			// What the real API answers for unknown files.
			writeJSON(w, 403, apiErrorBody(403, "PERMISSION_DENIED", "You do not have permission to access the File "+id+" or it may not exist."))
			return
		}
		switch r.Method {
		case http.MethodGet:
			f.gets[id]++
			if f.gets[id] > f.processingGets {
				file.State = genai.FileStateActive
			}
			writeJSON(w, 200, file)
		case http.MethodDelete:
			delete(f.files, id)
			writeJSON(w, 200, map[string]any{})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}

	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1beta/models/") && strings.HasSuffix(path, ":generateContent"):
		f.genBodies = append(f.genBodies, body)
		if len(f.genStatus) > 0 {
			code := f.genStatus[0]
			f.genStatus = f.genStatus[1:]
			if code != 200 {
				var details []map[string]any
				if code == 429 {
					details = append(details, map[string]any{
						"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "0.005s",
					})
				}
				writeJSON(w, code, apiErrorBody(code, http.StatusText(code), "scripted failure", details...))
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, f.genBody)

	default:
		f.t.Errorf("fakeGemini: unexpected request %s %s", r.Method, path)
		http.NotFound(w, r)
	}
}

const cannedGenerateResponse = `{
  "candidates": [{
    "content": {"role": "model", "parts": [{"text": "{\"gate\":{\"passed\":true,\"confidence\":0.93,\"reason\":\"A car.\",\"detectedSubject\":\"car\"},\"videoSummary\":\"Walkaround.\",\"scenes\":[]}"}]},
    "finishReason": "STOP",
    "index": 0
  }],
  "usageMetadata": {"promptTokenCount": 15234, "candidatesTokenCount": 212, "thoughtsTokenCount": 17, "totalTokenCount": 15463},
  "modelVersion": "gemini-3.5-flash-lite-preview-10-2026",
  "responseId": "resp-1"
}`

// getPath walks a decoded JSON value: keys for objects, ints for arrays.
func getPath(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			require.True(t, ok, "expected object at %v, got %T", p, v)
			v, ok = m[k]
			require.True(t, ok, "missing key %q in %v", k, m)
		case int:
			a, ok := v.([]any)
			require.True(t, ok, "expected array at %v, got %T", p, v)
			require.Greater(t, len(a), k)
			v = a[k]
		}
	}
	return v
}

func TestHTTPEndToEnd(t *testing.T) {
	fg := newFakeGemini(t)
	fg.genBody = cannedGenerateResponse
	fg.processingGets = 2 // PROCESSING twice, then ACTIVE
	c := fg.client(t)

	zero, one := 0.0, 1.0
	req := analyzeRequest(t)
	req.ResponseSchema = &analyzer.Schema{
		Type:             "object",
		PropertyOrdering: []string{"gate", "scenes"},
		Required:         []string{"gate", "scenes"},
		Properties: map[string]*analyzer.Schema{
			"gate": {Type: "object", Properties: map[string]*analyzer.Schema{
				"passed":     {Type: "boolean"},
				"confidence": {Type: "number", Minimum: &zero, Maximum: &one},
			}},
			"scenes": {Type: "array", Items: &analyzer.Schema{
				Type: "object", Properties: map[string]*analyzer.Schema{
					"shotId": {Type: "string", Enum: []string{"front_three_quarter", "other"}},
				},
			}},
		},
	}

	resp, err := c.Generate(context.Background(), req)
	require.NoError(t, err)

	// Response parsing through the real SDK.
	assert.JSONEq(t, `{"gate":{"passed":true,"confidence":0.93,"reason":"A car.","detectedSubject":"car"},"videoSummary":"Walkaround.","scenes":[]}`, resp.Text)
	assert.Equal(t, analyzer.Usage{
		Model: "gemini-3.5-flash-lite-preview-10-2026", InputTokens: 15234, OutputTokens: 212, ThinkingTokens: 17,
		Latency: resp.Usage.Latency,
	}, resp.Usage)
	assert.Positive(t, resp.Usage.Latency)
	require.Len(t, resp.FileHandles, 2)
	assert.Equal(t, "files/vid1", resp.FileHandles[0])
	assert.Empty(t, resp.FileHandles[1])

	fg.mu.Lock()
	uploaded := fg.files["vid1"]
	require.NotNil(t, uploaded)
	assert.Equal(t, "v2i-analyze-job_01HX", uploaded.DisplayName)
	assert.Equal(t, "video/mp4", uploaded.MIMEType)
	assert.Equal(t, int64(2048), *uploaded.SizeBytes, "the whole preview was uploaded")
	assert.Equal(t, 3, fg.gets["vid1"], "polled until ACTIVE")
	require.Len(t, fg.genBodies, 1)
	genBody := fg.genBodies[0]
	fg.mu.Unlock()

	// Request shape on the wire.
	var body map[string]any
	require.NoError(t, json.Unmarshal(genBody, &body))
	assert.Equal(t, req.SystemPrompt, getPath(t, body, "systemInstruction", "parts", 0, "text"))
	assert.Equal(t, "user", getPath(t, body, "contents", 0, "role"))
	assert.Equal(t, fg.srv.URL+"/v1beta/files/vid1", getPath(t, body, "contents", 0, "parts", 0, "fileData", "fileUri"))
	assert.Equal(t, "video/mp4", getPath(t, body, "contents", 0, "parts", 0, "fileData", "mimeType"))
	assert.Equal(t, 2.0, getPath(t, body, "contents", 0, "parts", 0, "videoMetadata", "fps"))
	assert.Equal(t, "Step 1 — Gate.", getPath(t, body, "contents", 0, "parts", 1, "text"))
	gc := getPath(t, body, "generationConfig")
	assert.Equal(t, "application/json", getPath(t, gc, "responseMimeType"))
	assert.InDelta(t, 0.2, getPath(t, gc, "temperature"), 1e-6)
	assert.Equal(t, 0.0, getPath(t, gc, "thinkingConfig", "thinkingBudget"), "a zero budget must be sent, not omitted")
	assert.Equal(t, 4096.0, getPath(t, gc, "maxOutputTokens"), "the answer is capped")
	assert.Equal(t, "OBJECT", getPath(t, gc, "responseSchema", "type"))
	assert.Equal(t, []any{"gate", "scenes"}, getPath(t, gc, "responseSchema", "propertyOrdering"))
	assert.Equal(t, []any{"gate", "scenes"}, getPath(t, gc, "responseSchema", "required"))
	assert.Equal(t, "NUMBER", getPath(t, gc, "responseSchema", "properties", "gate", "properties", "confidence", "type"))
	assert.Equal(t, 1.0, getPath(t, gc, "responseSchema", "properties", "gate", "properties", "confidence", "maximum"))
	assert.Equal(t, "ARRAY", getPath(t, gc, "responseSchema", "properties", "scenes", "type"))
	assert.Equal(t, []any{"front_three_quarter", "other"},
		getPath(t, gc, "responseSchema", "properties", "scenes", "items", "properties", "shotId", "enum"))

	// Retry of the same logical call reuses the handle: no second upload.
	req.Parts[0].FileHandle = resp.FileHandles[0]
	resp2, err := c.Generate(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, "files/vid1", resp2.FileHandles[0])
	fg.mu.Lock()
	assert.Equal(t, 1, fg.nextID, "the ACTIVE handle was reused")
	fg.mu.Unlock()

	// Release, then release again (unknown now → 403 → no-op).
	require.NoError(t, c.ReleaseFile(context.Background(), "files/vid1"))
	require.NoError(t, c.ReleaseFile(context.Background(), "files/vid1"))
	fg.mu.Lock()
	assert.Empty(t, fg.files)
	fg.mu.Unlock()

	assertNothingLeaks(t, fg)
}

// assertNothingLeaks checks no request carried the source name or the local file path.
func assertNothingLeaks(t *testing.T, fg *fakeGemini) {
	t.Helper()
	fg.mu.Lock()
	defer fg.mu.Unlock()
	require.NotEmpty(t, fg.requests)
	for _, r := range fg.requests {
		all := r.Method + " " + r.Path + "?" + r.Query + "\n" + string(r.Body)
		for k, vs := range r.Header {
			all += "\n" + k + ": " + strings.Join(vs, ",")
		}
		assert.NotContains(t, all, "SECRET", "SourceName leaked in %s %s", r.Method, r.Path)
		assert.NotContains(t, all, "preview.mp4", "local file name leaked in %s %s", r.Method, r.Path)
		assert.NotContains(t, all, "/var/folders", "local path leaked in %s %s", r.Method, r.Path)
	}
}

// Without a limit the field is omitted, so the API applies the model's own maximum.
func TestHTTPNoMaxOutputTokens(t *testing.T) {
	fg := newFakeGemini(t)
	fg.genBody = cannedGenerateResponse
	req := analyzeRequest(t)
	req.MaxOutputTokens = 0
	_, err := fg.client(t).Generate(context.Background(), req)
	require.NoError(t, err)
	fg.mu.Lock()
	require.Len(t, fg.genBodies, 1)
	genBody := fg.genBodies[0]
	fg.mu.Unlock()
	var body map[string]any
	require.NoError(t, json.Unmarshal(genBody, &body))
	gc, ok := getPath(t, body, "generationConfig").(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, gc, "maxOutputTokens")
}

func TestHTTPRetriesThenSucceeds(t *testing.T) {
	fg := newFakeGemini(t)
	fg.genBody = cannedGenerateResponse
	fg.genStatus = []int{503, 429, 200}
	c := fg.client(t)
	resp, err := c.Generate(context.Background(), analyzeRequest(t))
	require.NoError(t, err)
	assert.Equal(t, 15234, resp.Usage.InputTokens)
	fg.mu.Lock()
	defer fg.mu.Unlock()
	assert.Len(t, fg.genBodies, 3)
	assert.Equal(t, 1, fg.nextID, "retries reuse the uploaded file")
}

func TestHTTPGivesUp(t *testing.T) {
	fg := newFakeGemini(t)
	fg.genStatus = []int{500, 500, 500}
	c := fg.client(t)
	_, err := c.Generate(context.Background(), analyzeRequest(t))
	require.Error(t, err)
	assert.ErrorIs(t, err, analyzer.ErrUnavailable)
	ae, ok := apiError(err)
	require.True(t, ok)
	assert.Equal(t, 500, ae.Code)
	fg.mu.Lock()
	defer fg.mu.Unlock()
	assert.Len(t, fg.genBodies, 3)
	assert.Empty(t, fg.files, "the upload of a failed call is deleted")
}

func TestHTTPBadRequestNotRetried(t *testing.T) {
	fg := newFakeGemini(t)
	fg.genStatus = []int{400}
	_, err := fg.client(t).Generate(context.Background(), analyzeRequest(t))
	require.Error(t, err)
	assert.ErrorIs(t, err, analyzer.ErrUnavailable)
	assert.Contains(t, err.Error(), "request rejected (HTTP 400")
	fg.mu.Lock()
	defer fg.mu.Unlock()
	assert.Len(t, fg.genBodies, 1)
}

func TestHTTPUploadCreateRateLimited(t *testing.T) {
	fg := newFakeGemini(t)
	fg.genBody = cannedGenerateResponse
	fg.createErr = []int{429}
	resp, err := fg.client(t).Generate(context.Background(), analyzeRequest(t))
	require.NoError(t, err, "a 429 on upload start is retried even though the SDK drops its type")
	assert.Equal(t, "files/vid1", resp.FileHandles[0])

	fg2 := newFakeGemini(t)
	fg2.createErr = []int{429, 429, 429}
	_, err = fg2.client(t).Generate(context.Background(), analyzeRequest(t))
	assert.ErrorIs(t, err, analyzer.ErrUnavailable)
}

func TestHTTPStaleHandleIsReuploaded(t *testing.T) {
	fg := newFakeGemini(t)
	fg.genBody = cannedGenerateResponse
	req := analyzeRequest(t)
	req.Parts[0].FileHandle = "files/expired-long-ago"
	resp, err := fg.client(t).Generate(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, "files/vid1", resp.FileHandles[0])
}

func TestHTTPSweepFiles(t *testing.T) {
	fg := newFakeGemini(t)
	now := time.Now().UTC()
	fg.mu.Lock()
	fg.files["vid1"] = &genai.File{Name: "files/vid1", DisplayName: "v2i-analyze-job_a", CreateTime: now.Add(-10 * time.Hour)}
	fg.files["vid2"] = &genai.File{Name: "files/vid2", DisplayName: "other-app", CreateTime: now.Add(-10 * time.Hour)}
	fg.files["vid3"] = &genai.File{Name: "files/vid3", DisplayName: "v2i-analyze-job_b", CreateTime: now.Add(-time.Minute)}
	fg.files["vid4"] = &genai.File{Name: "files/vid4", DisplayName: "v2i-rerank-job_c", CreateTime: now.Add(-20 * time.Hour)}
	fg.nextID = 4
	fg.mu.Unlock()

	n, err := fg.client(t).SweepFiles(context.Background(), 6*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	fg.mu.Lock()
	defer fg.mu.Unlock()
	assert.ElementsMatch(t, []string{"vid2", "vid3"}, keys(fg.files))
	var sawSecondPage bool
	for _, r := range fg.requests {
		if r.Path == "/v1beta/files" && strings.Contains(r.Query, "pageToken=page2") {
			sawSecondPage = true
		}
	}
	assert.True(t, sawSecondPage, "the listing follows pagination")
}

func TestHTTPContextCanceled(t *testing.T) {
	fg := newFakeGemini(t)
	fg.processingGets = 1 << 30 // never ACTIVE
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := fg.client(t).Generate(ctx, analyzeRequest(t))
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "got %v", err)
	assert.Equal(t, context.DeadlineExceeded, err, "returned unwrapped")
	fg.mu.Lock()
	defer fg.mu.Unlock()
	assert.Empty(t, fg.files, "the upload is deleted after the context ends")
}

func keys[M ~map[string]V, V any](m M) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
