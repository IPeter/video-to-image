package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// ErrorCode is the stable machine string in every error envelope. The frontend maps
// codes to copy; Message is only a safe English default.
type ErrorCode string

const (
	CodeDomainNotFound    ErrorCode = "DOMAIN_NOT_FOUND"
	CodeOriginNotAllowed  ErrorCode = "ORIGIN_NOT_ALLOWED"
	CodeUnauthorized      ErrorCode = "UNAUTHORIZED"
	CodeInvalidRequest    ErrorCode = "INVALID_REQUEST"
	CodeUploadTooLarge    ErrorCode = "UPLOAD_TOO_LARGE"
	CodeUnsupportedFormat ErrorCode = "UNSUPPORTED_FORMAT"
	CodeCannotDecode      ErrorCode = "CANNOT_DECODE"
	CodeTooLong           ErrorCode = "TOO_LONG"
	CodeTooShort          ErrorCode = "TOO_SHORT"
	CodeResolutionTooLow  ErrorCode = "RESOLUTION_TOO_LOW"
	CodeResolutionTooHigh ErrorCode = "RESOLUTION_TOO_HIGH" // width×height > maxPixels, or fps > maxFps
	CodeServerBusy        ErrorCode = "SERVER_BUSY"         // too many concurrent uploads / scratch disk low (503)
	CodeRateLimited       ErrorCode = "RATE_LIMITED"
	CodeJobNotFound       ErrorCode = "JOB_NOT_FOUND"
	CodeImageNotFound     ErrorCode = "IMAGE_NOT_FOUND"
	CodeNotReady          ErrorCode = "NOT_READY"
	CodeExpired           ErrorCode = "EXPIRED"
	CodeModelUnavailable  ErrorCode = "MODEL_UNAVAILABLE"
	CodeModelBadOutput    ErrorCode = "MODEL_BAD_OUTPUT"
	CodeProcessingError   ErrorCode = "PROCESSING_ERROR"
	CodeNotFound          ErrorCode = "NOT_FOUND"
	CodeInternal          ErrorCode = "INTERNAL_ERROR"
)

// AllErrorCodes is the exhaustive enum published in the OpenAPI document.
var AllErrorCodes = []ErrorCode{
	CodeDomainNotFound, CodeOriginNotAllowed, CodeUnauthorized, CodeInvalidRequest,
	CodeUploadTooLarge, CodeUnsupportedFormat, CodeCannotDecode, CodeTooLong, CodeTooShort,
	CodeResolutionTooLow, CodeResolutionTooHigh, CodeServerBusy, CodeRateLimited, CodeJobNotFound, CodeImageNotFound, CodeNotReady,
	CodeExpired, CodeModelUnavailable, CodeModelBadOutput, CodeProcessingError, CodeNotFound,
	CodeInternal,
}

func (ErrorCode) Schema(huma.Registry) *huma.Schema {
	return enumSchema("Stable machine-readable error code.", AllErrorCodes...)
}

// ErrorDetail is the inner object of the envelope.
type ErrorDetail struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	Details any       `json:"details,omitempty"`
}

// APIError is the envelope { "error": { code, message, details? } } for every non-2xx
// response. It implements huma.StatusError so Huma handlers can return it directly.
type APIError struct {
	status int
	Err    ErrorDetail `json:"error"`
}

func (e *APIError) Error() string  { return string(e.Err.Code) + ": " + e.Err.Message }
func (e *APIError) GetStatus() int { return e.status }

// NewAPIError builds an envelope error.
func NewAPIError(status int, code ErrorCode, message string) *APIError {
	return &APIError{status: status, Err: ErrorDetail{Code: code, Message: message}}
}

// WithDetails attaches details.
func (e *APIError) WithDetails(d any) *APIError { e.Err.Details = d; return e }

// WriteError writes the envelope on a raw (non-Huma) route.
func WriteError(w http.ResponseWriter, e *APIError) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(e.status)
	_ = json.NewEncoder(w).Encode(e)
}

// codeForStatus picks the envelope code for errors Huma raises itself (validation,
// unknown routes, panics).
func codeForStatus(status int) ErrorCode {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return CodeInvalidRequest
	case http.StatusUnauthorized:
		return CodeUnauthorized
	case http.StatusForbidden:
		return CodeOriginNotAllowed
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusRequestEntityTooLarge:
		return CodeUploadTooLarge
	case http.StatusTooManyRequests:
		return CodeRateLimited
	case http.StatusGone:
		return CodeExpired
	default:
		return CodeInternal
	}
}

func init() {
	// We always send [] rather than null for empty lists, so document arrays as
	// non-nullable (the generated TypeScript types are then plain T[]).
	huma.DefaultArrayNullable = false

	// Every error Huma produces uses our envelope (and the OpenAPI error schema is
	// derived from this type).
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		var apiErr *APIError
		for _, err := range errs {
			if errors.As(err, &apiErr) {
				return apiErr
			}
		}
		e := NewAPIError(status, codeForStatus(status), msg)
		if len(errs) > 0 {
			details := make([]string, 0, len(errs))
			for _, err := range errs {
				if err != nil {
					details = append(details, err.Error())
				}
			}
			if len(details) > 0 && status < 500 {
				e.Err.Details = details
			}
		}
		return e
	}
}
