package gemini

import (
	"context"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/genai"
)

// fakeAPI is a scriptable in-memory stand-in for the genai SDK. Each hook receives the
// 1-based call number of its kind; nil hooks use sensible defaults (uploads and gets
// return ACTIVE files, generate returns okResponse).
type fakeAPI struct {
	mu sync.Mutex

	onUpload   func(n int, path, mimeType, display string) (*genai.File, error)
	onGet      func(n int, name string) (*genai.File, error)
	onDelete   func(name string) error
	onGenerate func(ctx context.Context, n int, contents []*genai.Content, cfg *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error)
	list       []*genai.File
	listErr    error

	uploads   []uploadRec
	gets      []string
	deletes   []string
	generates []generateRec
}

type uploadRec struct{ path, mime, display string }

type generateRec struct {
	model    string
	contents []*genai.Content
	cfg      *genai.GenerateContentConfig
}

func activeFile(name, mime string) *genai.File {
	return &genai.File{Name: name, URI: "https://fake.test/v1beta/" + name, MIMEType: mime, State: genai.FileStateActive}
}

func (f *fakeAPI) upload(_ context.Context, path, mimeType, display string) (*genai.File, error) {
	f.mu.Lock()
	f.uploads = append(f.uploads, uploadRec{path, mimeType, display})
	n := len(f.uploads)
	hook := f.onUpload
	f.mu.Unlock()
	if hook != nil {
		return hook(n, path, mimeType, display)
	}
	return activeFile(fmt.Sprintf("files/up%d", n), mimeType), nil
}

func (f *fakeAPI) getFile(_ context.Context, name string) (*genai.File, error) {
	f.mu.Lock()
	f.gets = append(f.gets, name)
	n := len(f.gets)
	hook := f.onGet
	f.mu.Unlock()
	if hook != nil {
		return hook(n, name)
	}
	return activeFile(name, "video/mp4"), nil
}

func (f *fakeAPI) deleteFile(_ context.Context, name string) error {
	f.mu.Lock()
	f.deletes = append(f.deletes, name)
	hook := f.onDelete
	f.mu.Unlock()
	if hook != nil {
		return hook(name)
	}
	return nil
}

func (f *fakeAPI) listFiles(context.Context) iter.Seq2[*genai.File, error] {
	return func(yield func(*genai.File, error) bool) {
		for _, file := range f.list {
			if !yield(file, nil) {
				return
			}
		}
		if f.listErr != nil {
			yield(nil, f.listErr)
		}
	}
}

func (f *fakeAPI) generate(ctx context.Context, model string, contents []*genai.Content, cfg *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	f.mu.Lock()
	f.generates = append(f.generates, generateRec{model, contents, cfg})
	n := len(f.generates)
	hook := f.onGenerate
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, n, contents, cfg)
	}
	return okResponse(`{"ok":true}`), nil
}

func (f *fakeAPI) counts() (uploads, gets, deletes, generates int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploads), len(f.gets), len(f.deletes), len(f.generates)
}

func (f *fakeAPI) deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletes...)
}

func okResponse(text string) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content:      &genai.Content{Role: "model", Parts: []*genai.Part{{Text: text}}},
			FinishReason: genai.FinishReasonStop,
		}},
		ModelVersion: "gemini-3.5-flash-lite-001",
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount: 12000, CandidatesTokenCount: 800, ThoughtsTokenCount: 40, TotalTokenCount: 12840,
		},
	}
}

func apiErr(code int, status string) error {
	return genai.APIError{Code: code, Status: status, Message: "scripted " + status}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testClient returns a client over fake with millisecond timings.
func testClient(fake *fakeAPI, mod ...func(*Options)) *Client {
	opts := Options{
		Logger:            discardLogger(),
		BackoffBase:       time.Millisecond,
		MaxBackoff:        4 * time.Millisecond,
		PollInterval:      time.Millisecond,
		FileActiveTimeout: time.Second,
	}
	for _, m := range mod {
		m(&opts)
	}
	return newClient(fake, opts)
}

// writeFile creates a file with the given size under t.TempDir().
func writeFile(t *testing.T, name string, size int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i)
	}
	require.NoError(t, os.WriteFile(p, data, 0o600))
	return p
}
