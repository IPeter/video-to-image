package gemini

import (
	"context"
	"fmt"
	"iter"
	"net/http"
	"os"
	"strconv"

	"google.golang.org/genai"
)

// api is the slice of the genai SDK the client uses. It exists so the retry, upload
// and polling logic can be tested without the network; genaiAPI is the real thing.
type api interface {
	upload(ctx context.Context, path, mimeType, displayName string) (*genai.File, error)
	getFile(ctx context.Context, name string) (*genai.File, error)
	deleteFile(ctx context.Context, name string) error
	listFiles(ctx context.Context) iter.Seq2[*genai.File, error]
	generate(ctx context.Context, model string, contents []*genai.Content, cfg *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error)
}

type genaiAPI struct {
	c *genai.Client
}

// upload streams the file with Files.Upload rather than Files.UploadFromPath, which
// would also send the local file name (X-Goog-Upload-File-Name).
func (g *genaiAPI) upload(ctx context.Context, path, mimeType, displayName string) (*genai.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, localErrorf("open media file: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, localErrorf("stat media file: %w", err)
	}
	if st.IsDir() {
		return nil, localErrorf("media file %s is a directory", path)
	}
	hdr := http.Header{}
	hdr.Set("X-Goog-Upload-Header-Content-Length", strconv.FormatInt(st.Size(), 10))
	file, err := g.c.Files.Upload(ctx, f, &genai.UploadFileConfig{
		MIMEType:    mimeType,
		DisplayName: displayName,
		HTTPOptions: &genai.HTTPOptions{Headers: hdr},
	})
	if err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	return file, nil
}

func (g *genaiAPI) getFile(ctx context.Context, name string) (*genai.File, error) {
	return g.c.Files.Get(ctx, name, nil)
}

func (g *genaiAPI) deleteFile(ctx context.Context, name string) error {
	_, err := g.c.Files.Delete(ctx, name, nil)
	return err
}

func (g *genaiAPI) listFiles(ctx context.Context) iter.Seq2[*genai.File, error] {
	return g.c.Files.All(ctx)
}

func (g *genaiAPI) generate(ctx context.Context, model string, contents []*genai.Content, cfg *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	return g.c.Models.GenerateContent(ctx, model, contents, cfg)
}
