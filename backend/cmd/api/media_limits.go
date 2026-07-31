package main

import (
	"fmt"
	"net/http"
	"strings"
)

const (
	maxImageBytes    = 5 * 1024 * 1024
	maxDocumentBytes = 100 * 1024 * 1024
)

var allowedImageMIME = map[string]struct{}{
	"image/jpeg": {},
	"image/png":  {},
}

var allowedDocumentMIME = map[string]struct{}{
	"application/pdf":    {},
	"application/msword": {},
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   {},
	"application/vnd.ms-excel": {},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         {},
	"application/vnd.ms-powerpoint": {},
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": {},
	"text/plain": {},
}

func validateImageUpload(mimeType string, size int) error {
	return validateOutboundMedia("image", mimeType, size)
}

func validateDocumentUpload(mimeType string, size int) error {
	return validateOutboundMedia("document", mimeType, size)
}

func validateOutboundMedia(kind, mimeType string, size int) error {
	kind = strings.ToLower(strings.TrimSpace(kind))
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	if mimeType == "" {
		return mediaValidationError{code: "unsupported_type", message: "mime type is required"}
	}
	switch kind {
	case "image":
		if _, ok := allowedImageMIME[mimeType]; !ok {
			return mediaValidationError{code: "unsupported_type", message: fmt.Sprintf("unsupported image type %q", mimeType)}
		}
		if size > maxImageBytes {
			return mediaValidationError{code: "media_too_large", message: fmt.Sprintf("image exceeds %d bytes", maxImageBytes)}
		}
	case "document":
		if _, ok := allowedDocumentMIME[mimeType]; !ok {
			return mediaValidationError{code: "unsupported_type", message: fmt.Sprintf("unsupported document type %q", mimeType)}
		}
		if size > maxDocumentBytes {
			return mediaValidationError{code: "media_too_large", message: fmt.Sprintf("document exceeds %d bytes", maxDocumentBytes)}
		}
	default:
		return mediaValidationError{code: "unsupported_type", message: fmt.Sprintf("unsupported media kind %q", kind)}
	}
	if size <= 0 {
		return mediaValidationError{code: "unsupported_type", message: "empty media body"}
	}
	return nil
}

type mediaValidationError struct {
	code    string
	message string
}

func (e mediaValidationError) Error() string { return e.message }
func (e mediaValidationError) Code() string  { return e.code }

func writeMediaValidationError(w http.ResponseWriter, err error) {
	if ve, ok := err.(mediaValidationError); ok {
		writeJSON(w, http.StatusUnprocessableEntity, structuredError{Error: ve.message, Code: ve.code})
		return
	}
	writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: err.Error()})
}
