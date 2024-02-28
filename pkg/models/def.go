/*
 * @Author: rui.li
 * @Date: 2024-02-28 09:56:40
 * @LastEditors: rui.li
 * @LastEditTime: 2024-02-28 10:24:43
 * @FilePath: /DynamicBucketTusd/pkg/models/def.go
 */
package models

import (
	"net/http"
	"regexp"
)

const UploadLengthDeferred = "1"
const CurrentUploadDraftInteropVersion = "4"

var (
	ReExtractFileID  = regexp.MustCompile(`([^/]+)\/?$`)
	ReForwardedHost  = regexp.MustCompile(`host="?([^;"]+)`)
	ReForwardedProto = regexp.MustCompile(`proto=(https?)`)
	ReMimeType       = regexp.MustCompile(`^[a-z]+\/[a-z0-9\-\+\.]+$`)
)

var (
	ErrUnsupportedVersion               = NewError("ERR_UNSUPPORTED_VERSION", "missing, invalid or unsupported Tus-Resumable header", http.StatusPreconditionFailed)
	ErrMaxSizeExceeded                  = NewError("ERR_MAX_SIZE_EXCEEDED", "maximum size exceeded", http.StatusRequestEntityTooLarge)
	ErrInvalidContentType               = NewError("ERR_INVALID_CONTENT_TYPE", "missing or invalid Content-Type header", http.StatusBadRequest)
	ErrInvalidUploadLength              = NewError("ERR_INVALID_UPLOAD_LENGTH", "missing or invalid Upload-Length header", http.StatusBadRequest)
	ErrInvalidOffset                    = NewError("ERR_INVALID_OFFSET", "missing or invalid Upload-Offset header", http.StatusBadRequest)
	ErrNotFound                         = NewError("ERR_UPLOAD_NOT_FOUND", "upload not found", http.StatusNotFound)
	ErrFileLocked                       = NewError("ERR_UPLOAD_LOCKED", "file currently locked", http.StatusLocked)
	ErrLockTimeout                      = NewError("ERR_LOCK_TIMEOUT", "failed to acquire lock before timeout", http.StatusInternalServerError)
	ErrMismatchOffset                   = NewError("ERR_MISMATCHED_OFFSET", "mismatched offset", http.StatusConflict)
	ErrSizeExceeded                     = NewError("ERR_UPLOAD_SIZE_EXCEEDED", "upload's size exceeded", http.StatusRequestEntityTooLarge)
	ErrNotImplemented                   = NewError("ERR_NOT_IMPLEMENTED", "feature not implemented", http.StatusNotImplemented)
	ErrUploadNotFinished                = NewError("ERR_UPLOAD_NOT_FINISHED", "one of the partial uploads is not finished", http.StatusBadRequest)
	ErrInvalidConcat                    = NewError("ERR_INVALID_CONCAT", "invalid Upload-Concat header", http.StatusBadRequest)
	ErrModifyFinal                      = NewError("ERR_MODIFY_FINAL", "modifying a final upload is not allowed", http.StatusForbidden)
	ErrUploadLengthAndUploadDeferLength = NewError("ERR_AMBIGUOUS_UPLOAD_LENGTH", "provided both Upload-Length and Upload-Defer-Length", http.StatusBadRequest)
	ErrInvalidUploadDeferLength         = NewError("ERR_INVALID_UPLOAD_LENGTH_DEFER", "invalid Upload-Defer-Length header", http.StatusBadRequest)
	ErrUploadStoppedByServer            = NewError("ERR_UPLOAD_STOPPED", "upload has been stopped by server", http.StatusBadRequest)
	ErrUploadRejectedByServer           = NewError("ERR_UPLOAD_REJECTED", "upload creation has been rejected by server", http.StatusBadRequest)
	ErrUploadInterrupted                = NewError("ERR_UPLOAD_INTERRUPTED", "upload has been interrupted by another request for this upload resource", http.StatusBadRequest)
	ErrServerShutdown                   = NewError("ERR_SERVER_SHUTDOWN", "request has been interrupted because the server is shutting down", http.StatusServiceUnavailable)
	ErrOriginNotAllowed                 = NewError("ERR_ORIGIN_NOT_ALLOWED", "request origin is not allowed", http.StatusForbidden)

	// These two responses are 500 for backwards compatability. Clients might receive a timeout response
	// when the upload got interrupted. Most clients will not retry 4XX but only 5XX, so we responsd with 500 here.
	ErrReadTimeout     = NewError("ERR_READ_TIMEOUT", "timeout while reading request body", http.StatusInternalServerError)
	ErrConnectionReset = NewError("ERR_CONNECTION_RESET", "TCP connection reset by peer", http.StatusInternalServerError)
)
