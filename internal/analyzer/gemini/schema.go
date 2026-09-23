package gemini

import (
	"fmt"
	"slices"
	"strings"

	"google.golang.org/genai"

	"video-to-image/internal/analyzer"
)

// maxSchemaDepth guards against accidentally cyclic schemas.
const maxSchemaDepth = 32

// ConvertSchema converts the provider-neutral schema into a genai.Schema. Type names are
// case-insensitive ("object" → OBJECT) and an empty type is left unset. Enum values on
// a string also set Format "enum", as the Gemini API documents. Required and
// PropertyOrdering must name declared properties. A nil schema converts to nil.
func ConvertSchema(s *analyzer.Schema) (*genai.Schema, error) {
	out, err := convertSchema(s, "$", 0)
	if err != nil {
		return nil, fmt.Errorf("gemini: response schema: %w", err)
	}
	return out, nil
}

func convertSchema(s *analyzer.Schema, path string, depth int) (*genai.Schema, error) {
	if s == nil {
		return nil, nil
	}
	if depth > maxSchemaDepth {
		return nil, fmt.Errorf("%s: nested deeper than %d levels", path, maxSchemaDepth)
	}
	out := &genai.Schema{
		Description:      s.Description,
		PropertyOrdering: slices.Clone(s.PropertyOrdering),
		Required:         slices.Clone(s.Required),
		Enum:             slices.Clone(s.Enum),
	}
	switch strings.ToLower(strings.TrimSpace(s.Type)) {
	case "":
	case "object":
		out.Type = genai.TypeObject
	case "array":
		out.Type = genai.TypeArray
	case "string":
		out.Type = genai.TypeString
	case "number":
		out.Type = genai.TypeNumber
	case "integer":
		out.Type = genai.TypeInteger
	case "boolean":
		out.Type = genai.TypeBoolean
	default:
		return nil, fmt.Errorf("%s: unsupported type %q", path, s.Type)
	}
	if s.Nullable {
		out.Nullable = genai.Ptr(true)
	}
	if s.Minimum != nil {
		out.Minimum = genai.Ptr(*s.Minimum)
	}
	if s.Maximum != nil {
		out.Maximum = genai.Ptr(*s.Maximum)
	}
	if s.Minimum != nil && s.Maximum != nil && *s.Minimum > *s.Maximum {
		return nil, fmt.Errorf("%s: minimum %v is greater than maximum %v", path, *s.Minimum, *s.Maximum)
	}
	if len(out.Enum) > 0 && out.Type == genai.TypeString {
		out.Format = "enum"
	}
	if len(s.Properties) > 0 {
		out.Properties = make(map[string]*genai.Schema, len(s.Properties))
		for name, prop := range s.Properties {
			if prop == nil {
				return nil, fmt.Errorf("%s.%s: nil property schema", path, name)
			}
			c, err := convertSchema(prop, path+"."+name, depth+1)
			if err != nil {
				return nil, err
			}
			out.Properties[name] = c
		}
	}
	for _, list := range []struct {
		field string
		names []string
	}{{"required", s.Required}, {"propertyOrdering", s.PropertyOrdering}} {
		for _, name := range list.names {
			if _, ok := s.Properties[name]; !ok {
				return nil, fmt.Errorf("%s: %s names undeclared property %q", path, list.field, name)
			}
		}
	}
	if s.Items != nil {
		items, err := convertSchema(s.Items, path+"[]", depth+1)
		if err != nil {
			return nil, err
		}
		out.Items = items
	}
	return out, nil
}
