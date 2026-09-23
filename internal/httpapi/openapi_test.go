package httpapi

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAPISpec(t *testing.T) {
	raw, err := OpenAPISpec()
	require.NoError(t, err)

	var doc struct {
		OpenAPI string                                `json:"openapi"`
		Paths   map[string]map[string]json.RawMessage `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	assert.Equal(t, "3.1.0", doc.OpenAPI)

	var opIDs []string
	for _, methods := range doc.Paths {
		for _, op := range methods {
			var o struct {
				OperationID string `json:"operationId"`
			}
			require.NoError(t, json.Unmarshal(op, &o))
			opIDs = append(opIDs, o.OperationID)
		}
	}
	sort.Strings(opIDs)
	assert.Equal(t, []string{
		"abortUpload", "completeUpload", "createJob", "createUpload", "deleteJob", "downloadZip",
		"getDomain", "getImage", "getJob", "getThumbnail", "getUsage", "putUploadChunk",
	}, opIDs)
}
