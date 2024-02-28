package handler

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/susufqx/dynamic-bucket-tusd/pkg/config"
	"github.com/susufqx/dynamic-bucket-tusd/pkg/models"
	"github.com/susufqx/dynamic-bucket-tusd/pkg/s3store"
	"golang.org/x/exp/slog"
)

// UnroutedHandler exposes methods to handle requests as part of the tus protocol,
// such as PostFile, HeadFile, PatchFile and DelFile. In addition the GetFile method
// is provided which is, however, not part of the specification.
type UnroutedHandler struct {
	config        config.Config
	composer      *models.StoreComposer
	isBasePathAbs bool
	basePath      string
	logger        *slog.Logger
	extensions    string

	// CompleteUploads is used to send notifications whenever an upload is
	// completed by a user. The HookEvent will contain information about this
	// upload after it is completed. Sending to this channel will only
	// happen if the NotifyCompleteUploads field is set to true in the Config
	// structure. Notifications will also be sent for completions using the
	// Concatenation extension.
	CompleteUploads chan models.HookEvent
	// TerminatedUploads is used to send notifications whenever an upload is
	// terminated by a user. The HookEvent will contain information about this
	// upload gathered before the termination. Sending to this channel will only
	// happen if the NotifyTerminatedUploads field is set to true in the Config
	// structure.
	TerminatedUploads chan models.HookEvent
	// UploadProgress is used to send notifications about the progress of the
	// currently running uploads. For each open PATCH request, every second
	// a HookEvent instance will be send over this channel with the Offset field
	// being set to the number of bytes which have been transfered to the server.
	// Please be aware that this number may be higher than the number of bytes
	// which have been stored by the data store! Sending to this channel will only
	// happen if the NotifyUploadProgress field is set to true in the Config
	// structure.
	UploadProgress chan models.HookEvent
	// CreatedUploads is used to send notifications about the uploads having been
	// created. It triggers post creation and therefore has all the HookEvent incl.
	// the ID available already. It facilitates the post-create hook. Sending to
	// this channel will only happen if the NotifyCreatedUploads field is set to
	// true in the Config structure.
	CreatedUploads chan models.HookEvent
	// Metrics provides numbers of the usage for this handler.
	Metrics models.Metrics
}

// NewUnroutedHandler creates a new handler without routing using the given
// configuration. It exposes the http handlers which need to be combined with
// a router (aka mux) of your choice. If you are looking for preconfigured
// handler see NewHandler.
func NewUnroutedHandler(config config.Config) (*UnroutedHandler, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	// Only promote extesions using the Tus-Extension header which are implemented
	extensions := "creation,creation-with-upload"
	if config.StoreComposer.UsesTerminater {
		extensions += ",termination"
	}
	if config.StoreComposer.UsesConcater {
		extensions += ",concatenation"
	}
	if config.StoreComposer.UsesLengthDeferrer {
		extensions += ",creation-defer-length"
	}

	handler := &UnroutedHandler{
		config:            config,
		composer:          config.StoreComposer,
		basePath:          config.BasePath,
		isBasePathAbs:     config.IsAbs,
		CompleteUploads:   make(chan models.HookEvent),
		TerminatedUploads: make(chan models.HookEvent),
		UploadProgress:    make(chan models.HookEvent),
		CreatedUploads:    make(chan models.HookEvent),
		logger:            config.Logger,
		extensions:        extensions,
		Metrics:           models.NewMetrics(),
	}

	return handler, nil
}

// SupportedExtensions returns a comma-separated list of the supported tus extensions.
// The availability of an extension usually depends on whether the provided data store
// implements some additional interfaces.
func (handler *UnroutedHandler) SupportedExtensions() string {
	return handler.extensions
}

// Middleware checks various aspects of the request and ensures that it
// conforms with the spec. Also handles method overriding for clients which
// cannot make PATCH AND DELETE requests. If you are using the tusd handlers
// directly you will need to wrap at least the POST and PATCH endpoints in
// this middleware.
func (handler *UnroutedHandler) Middleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Construct our own context and make it available in the request. Successive logic
		// should use handler.getContext to retrieve it
		c := handler.newContext(w, r)
		r = r.WithContext(c)

		// Set the initial read deadline for consuming the request body. All headers have already been read,
		// so this is only for reading the request body. While reading, we regularly update the read deadline
		// so this deadline is usually not final. See the BodyReader and writeChunk.
		// We also update the write deadline, but makes sure that it is larger than the read deadline, so we
		// can still write a response in the case of a read timeout.
		if err := c.GetResC().SetReadDeadline(time.Now().Add(handler.config.NetworkTimeout)); err != nil {
			c.Log.Warn("NetworkControlError", "error", err)
		}
		if err := c.GetResC().SetWriteDeadline(time.Now().Add(2 * handler.config.NetworkTimeout)); err != nil {
			c.Log.Warn("NetworkControlError", "error", err)
		}

		// Allow overriding the HTTP method. The reason for this is
		// that some libraries/environments do not support PATCH and
		// DELETE requests, e.g. Flash in a browser and parts of Java.
		if newMethod := r.Header.Get("X-HTTP-Method-Override"); r.Method == "POST" && newMethod != "" {
			r.Method = newMethod
		}

		c.Log.Info("RequestIncoming")

		handler.Metrics.IncRequestsTotal(r.Method)

		header := w.Header()

		cors := handler.config.Cors
		if origin := r.Header.Get("Origin"); !cors.Disable && origin != "" {
			originIsAllowed := cors.AllowOrigin.MatchString(origin)
			if !originIsAllowed {
				handler.sendError(c, models.ErrOriginNotAllowed)
				return
			}

			header.Set("Access-Control-Allow-Origin", origin)
			header.Set("Vary", "Origin")

			if cors.AllowCredentials {
				header.Add("Access-Control-Allow-Credentials", "true")
			}

			if r.Method == "OPTIONS" {
				// Preflight request
				header.Add("Access-Control-Allow-Methods", cors.AllowMethods)
				header.Add("Access-Control-Allow-Headers", cors.AllowHeaders)
				header.Set("Access-Control-Max-Age", cors.MaxAge)
			} else {
				// Actual request
				header.Add("Access-Control-Expose-Headers", cors.ExposeHeaders)
			}
		}

		// Detect requests with tus v1 protocol vs the IETF resumable upload draft
		isTusV1 := !handler.isResumableUploadDraftRequest(r)

		if isTusV1 {
			// Set current version used by the server
			header.Set("Tus-Resumable", "1.0.0")
		}

		// Add nosniff to all responses https://golang.org/src/net/http/server.go#L1429
		header.Set("X-Content-Type-Options", "nosniff")

		// Set appropriated headers in case of OPTIONS method allowing protocol
		// discovery and end with an 204 No Content
		if r.Method == "OPTIONS" {
			if handler.config.MaxSize > 0 {
				header.Set("Tus-Max-Size", strconv.FormatInt(handler.config.MaxSize, 10))
			}

			header.Set("Tus-Version", "1.0.0")
			header.Set("Tus-Extension", handler.extensions)

			// Although the 204 No Content status code is a better fit in this case,
			// since we do not have a response body included, we cannot use it here
			// as some browsers only accept 200 OK as successful response to a
			// preflight request. If we send them the 204 No Content the response
			// will be ignored or interpreted as a rejection.
			// For example, the Presto engine, which is used in older versions of
			// Opera, Opera Mobile and Opera Mini, handles CORS this way.
			handler.sendResp(c, models.HTTPResponse{
				StatusCode: http.StatusOK,
			})
			return
		}

		// Test if the version sent by the client is supported
		// GET and HEAD methods are not checked since a browser may visit this URL and does
		// not include this header. GET requests are not part of the specification.
		if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("Tus-Resumable") != "1.0.0" && isTusV1 {
			handler.sendError(c, models.ErrUnsupportedVersion)
			return
		}

		// Proceed with routing the request
		h.ServeHTTP(w, r)
	})
}

// PostFile creates a new file upload using the datastore after validating the
// length and parsing the metadata.
func (handler *UnroutedHandler) PostFile(w http.ResponseWriter, r *http.Request) {
	bucketName := r.Header.Get("bucket-name")
	endpoint := r.Header.Get("endpoint")
	s3c := handler.config.Service
	if endpoint != "" {
		s3c = s3.New(s3.Options{
			Region: "",
			Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
				handler.config.S3Key,
				handler.config.S3Secret,
				"")),
			BaseEndpoint: &endpoint,
			UsePathStyle: true,
		})
	}

	if bucketName != "" {
		store := s3store.New(bucketName, s3c)
		composer := models.NewStoreComposer()
		store.UseIn(composer)
		handler.composer = composer
	}

	if handler.isResumableUploadDraftRequest(r) {
		handler.PostFileV2(w, r)
		return
	}

	c := handler.getContext(w, r)

	// Check for presence of application/offset+octet-stream. If another content
	// type is defined, it will be ignored and treated as none was set because
	// some HTTP clients may enforce a default value for this header.
	containsChunk := r.Header.Get("Content-Type") == "application/offset+octet-stream"

	// Only use the proper Upload-Concat header if the concatenation extension
	// is even supported by the data store.
	var concatHeader string
	if handler.composer.UsesConcater {
		concatHeader = r.Header.Get("Upload-Concat")
	}

	// Parse Upload-Concat header
	isPartial, isFinal, partialUploadIDs, err := parseConcat(concatHeader)
	if err != nil {
		handler.sendError(c, err)
		return
	}

	// If the upload is a final upload created by concatenation multiple partial
	// uploads the size is sum of all sizes of these files (no need for
	// Upload-Length header)
	var size int64
	var sizeIsDeferred bool
	var partialUploads []models.Upload
	if isFinal {
		// A final upload must not contain a chunk within the creation request
		if containsChunk {
			handler.sendError(c, models.ErrModifyFinal)
			return
		}

		partialUploads, size, err = handler.sizeOfUploads(c, partialUploadIDs)
		if err != nil {
			handler.sendError(c, err)
			return
		}
	} else {
		uploadLengthHeader := r.Header.Get("Upload-Length")
		uploadDeferLengthHeader := r.Header.Get("Upload-Defer-Length")
		size, sizeIsDeferred, err = handler.validateNewUploadLengthHeaders(uploadLengthHeader, uploadDeferLengthHeader)
		if err != nil {
			handler.sendError(c, err)
			return
		}
	}

	// Test whether the size is still allowed
	if handler.config.MaxSize > 0 && size > handler.config.MaxSize {
		handler.sendError(c, models.ErrMaxSizeExceeded)
		return
	}

	// Parse metadata
	meta := ParseMetadataHeader(r.Header.Get("Upload-Metadata"))

	info := models.FileInfo{
		Size:           size,
		SizeIsDeferred: sizeIsDeferred,
		MetaData:       meta,
		IsPartial:      isPartial,
		IsFinal:        isFinal,
		PartialUploads: partialUploadIDs,
	}

	resp := models.HTTPResponse{
		StatusCode: http.StatusCreated,
		Header:     models.HTTPHeader{},
	}

	if handler.config.PreUploadCreateCallback != nil {
		resp2, changes, err := handler.config.PreUploadCreateCallback(models.NewHookEvent(c, info))
		if err != nil {
			handler.sendError(c, err)
			return
		}
		resp = resp.MergeWith(resp2)

		// Apply changes returned from the pre-create hook.
		if changes.ID != "" {
			info.ID = changes.ID
		}

		if changes.MetaData != nil {
			info.MetaData = changes.MetaData
		}

		if changes.Storage != nil {
			info.Storage = changes.Storage
		}
	}

	upload, err := handler.composer.Core.NewUpload(c, info)
	if err != nil {
		handler.sendError(c, err)
		return
	}

	info, err = upload.GetInfo(c)
	if err != nil {
		handler.sendError(c, err)
		return
	}

	id := info.ID

	// Add the Location header directly after creating the new resource to even
	// include it in cases of failure when an error is returned
	url := handler.absFileURL(r, id)
	resp.Header["Location"] = url

	handler.Metrics.IncUploadsCreated()
	c.Log = c.Log.With("id", id)
	c.Log.Info("UploadCreated", "id", id, "size", size, "url", url)

	if handler.config.NotifyCreatedUploads {
		handler.CreatedUploads <- models.NewHookEvent(c, info)
	}

	if isFinal {
		concatableUpload := handler.composer.Concater.AsConcatableUpload(upload)
		if err := concatableUpload.ConcatUploads(c, partialUploads); err != nil {
			handler.sendError(c, err)
			return
		}
		info.Offset = size

		if handler.config.NotifyCompleteUploads {
			handler.CompleteUploads <- models.NewHookEvent(c, info)
		}
	}

	if containsChunk {
		if handler.composer.UsesLocker {
			lock, err := handler.lockUpload(c, id)
			if err != nil {
				handler.sendError(c, err)
				return
			}

			defer lock.Unlock()
		}

		resp, err = handler.writeChunk(c, resp, upload, info)
		if err != nil {
			handler.sendError(c, err)
			return
		}
	} else if !sizeIsDeferred && size == 0 {
		// Directly finish the upload if the upload is empty (i.e. has a size of 0).
		// This statement is in an else-if block to avoid causing duplicate calls
		// to finishUploadIfComplete if an upload is empty and contains a chunk.
		resp, err = handler.finishUploadIfComplete(c, resp, upload, info)
		if err != nil {
			handler.sendError(c, err)
			return
		}

	}

	handler.sendResp(c, resp)
}

// PostFile creates a new file upload using the datastore after validating the
// length and parsing the metadata.
func (handler *UnroutedHandler) PostFileV2(w http.ResponseWriter, r *http.Request) {
	c := handler.getContext(w, r)

	// Parse headers
	contentType := r.Header.Get("Content-Type")
	contentDisposition := r.Header.Get("Content-Disposition")
	isComplete := r.Header.Get("Upload-Complete") == "?1"

	info := models.FileInfo{
		MetaData: make(models.MetaData),
	}
	if isComplete && r.ContentLength != -1 {
		// If the client wants to perform the upload in one request with Content-Length, we know the final upload size.
		info.Size = r.ContentLength
	} else {
		// Error out if the storage does not support upload length deferring, but we need it.
		if !handler.composer.UsesLengthDeferrer {
			handler.sendError(c, models.ErrNotImplemented)
			return
		}

		info.SizeIsDeferred = true
	}

	// Parse Content-Type and Content-Disposition to get file type or file name
	if contentType != "" {
		fileType, _, err := mime.ParseMediaType(contentType)
		if err != nil {
			handler.sendError(c, err)
			return
		}

		info.MetaData["filetype"] = fileType
	}

	if contentDisposition != "" {
		_, values, err := mime.ParseMediaType(contentDisposition)
		if err != nil {
			handler.sendError(c, err)
			return
		}

		if values["filename"] != "" {
			info.MetaData["filename"] = values["filename"]
		}
	}

	resp := models.HTTPResponse{
		StatusCode: http.StatusCreated,
		Header:     models.HTTPHeader{},
	}

	// 1. Create upload resource
	if handler.config.PreUploadCreateCallback != nil {
		resp2, changes, err := handler.config.PreUploadCreateCallback(models.NewHookEvent(c, info))
		if err != nil {
			handler.sendError(c, err)
			return
		}
		resp = resp.MergeWith(resp2)

		// Apply changes returned from the pre-create hook.
		if changes.ID != "" {
			info.ID = changes.ID
		}

		if changes.MetaData != nil {
			info.MetaData = changes.MetaData
		}

		if changes.Storage != nil {
			info.Storage = changes.Storage
		}
	}

	upload, err := handler.composer.Core.NewUpload(c, info)
	if err != nil {
		handler.sendError(c, err)
		return
	}

	info, err = upload.GetInfo(c)
	if err != nil {
		handler.sendError(c, err)
		return
	}

	id := info.ID
	url := handler.absFileURL(r, id)
	resp.Header["Location"] = url

	// Send 104 response
	w.Header().Set("Location", url)
	w.Header().Set("Upload-Draft-Interop-Version", models.CurrentUploadDraftInteropVersion)
	w.WriteHeader(104)

	handler.Metrics.IncUploadsCreated()
	c.Log = c.Log.With("id", id)
	c.Log.Info("UploadCreated", "size", info.Size, "url", url)

	if handler.config.NotifyCreatedUploads {
		handler.CreatedUploads <- models.NewHookEvent(c, info)
	}

	// 2. Lock upload
	if handler.composer.UsesLocker {
		lock, err := handler.lockUpload(c, id)
		if err != nil {
			handler.sendError(c, err)
			return
		}

		defer lock.Unlock()
	}

	// 3. Write chunk
	resp, err = handler.writeChunk(c, resp, upload, info)
	if err != nil {
		handler.sendError(c, err)
		return
	}

	// 4. Finish upload, if necessary
	if isComplete && info.SizeIsDeferred {
		info, err = upload.GetInfo(c)
		if err != nil {
			handler.sendError(c, err)
			return
		}

		uploadLength := info.Offset

		lengthDeclarableUpload := handler.composer.LengthDeferrer.AsLengthDeclarableUpload(upload)
		if err := lengthDeclarableUpload.DeclareLength(c, uploadLength); err != nil {
			handler.sendError(c, err)
			return
		}

		info.Size = uploadLength
		info.SizeIsDeferred = false

		resp, err = handler.finishUploadIfComplete(c, resp, upload, info)
		if err != nil {
			handler.sendError(c, err)
			return
		}

	}

	handler.sendResp(c, resp)
}

// HeadFile returns the length and offset for the HEAD request
func (handler *UnroutedHandler) HeadFile(w http.ResponseWriter, r *http.Request) {
	bucketName := r.Header.Get("bucket-name")
	endpoint := r.Header.Get("endpoint")
	s3c := handler.config.Service
	if endpoint != "" {
		s3c = s3.New(s3.Options{
			Region: "",
			Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
				handler.config.S3Key,
				handler.config.S3Secret,
				"")),
			BaseEndpoint: &endpoint,
			UsePathStyle: true,
		})
	}

	if bucketName != "" {
		store := s3store.New(bucketName, s3c)
		composer := models.NewStoreComposer()
		store.UseIn(composer)
		handler.composer = composer
	}

	c := handler.getContext(w, r)

	id, err := extractIDFromPath(r.URL.Path)
	if err != nil {
		handler.sendError(c, err)
		return
	}
	c.Log = c.Log.With("id", id)

	if handler.composer.UsesLocker {
		lock, err := handler.lockUpload(c, id)
		if err != nil {
			handler.sendError(c, err)
			return
		}

		defer lock.Unlock()
	}

	upload, err := handler.composer.Core.GetUpload(c, id)
	if err != nil {
		handler.sendError(c, err)
		return
	}

	info, err := upload.GetInfo(c)
	if err != nil {
		handler.sendError(c, err)
		return
	}

	resp := models.HTTPResponse{
		Header: models.HTTPHeader{
			"Cache-Control": "no-store",
			"Upload-Offset": strconv.FormatInt(info.Offset, 10),
		},
	}

	if !handler.isResumableUploadDraftRequest(r) {
		// Add Upload-Concat header if possible
		if info.IsPartial {
			resp.Header["Upload-Concat"] = "partial"
		}

		if info.IsFinal {
			v := "final;"
			for _, uploadID := range info.PartialUploads {
				v += handler.absFileURL(r, uploadID) + " "
			}
			// Remove trailing space
			v = v[:len(v)-1]

			resp.Header["Upload-Concat"] = v
		}

		if len(info.MetaData) != 0 {
			resp.Header["Upload-Metadata"] = SerializeMetadataHeader(info.MetaData)
		}

		if info.SizeIsDeferred {
			resp.Header["Upload-Defer-Length"] = models.UploadLengthDeferred
		} else {
			resp.Header["Upload-Length"] = strconv.FormatInt(info.Size, 10)
			resp.Header["Content-Length"] = strconv.FormatInt(info.Size, 10)
		}

		resp.StatusCode = http.StatusOK
	} else {
		if !info.SizeIsDeferred && info.Offset == info.Size {
			// Upload is complete if we know the size and it matches the offset.
			resp.Header["Upload-Complete"] = "?1"
		} else {
			resp.Header["Upload-Complete"] = "?0"
		}

		resp.Header["Upload-Draft-Interop-Version"] = models.CurrentUploadDraftInteropVersion

		// Draft requires a 204 No Content response
		resp.StatusCode = http.StatusNoContent
	}

	handler.sendResp(c, resp)
}

// PatchFile adds a chunk to an upload. This operation is only allowed
// if enough space in the upload is left.
func (handler *UnroutedHandler) PatchFile(w http.ResponseWriter, r *http.Request) {
	bucketName := r.Header.Get("bucket-name")
	endpoint := r.Header.Get("endpoint")
	s3c := handler.config.Service
	if endpoint != "" {
		s3c = s3.New(s3.Options{
			Region: "",
			Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
				handler.config.S3Key,
				handler.config.S3Secret,
				"")),
			BaseEndpoint: &endpoint,
			UsePathStyle: true,
		})
	}

	if bucketName != "" {
		store := s3store.New(bucketName, s3c)
		composer := models.NewStoreComposer()
		store.UseIn(composer)
		handler.composer = composer
	}

	c := handler.getContext(w, r)

	isTusV1 := !handler.isResumableUploadDraftRequest(r)

	// Check for presence of application/offset+octet-stream
	if isTusV1 && r.Header.Get("Content-Type") != "application/offset+octet-stream" {
		handler.sendError(c, models.ErrInvalidContentType)
		return
	}

	// Check for presence of a valid Upload-Offset Header
	offset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || offset < 0 {
		handler.sendError(c, models.ErrInvalidOffset)
		return
	}

	id, err := extractIDFromPath(r.URL.Path)
	if err != nil {
		handler.sendError(c, err)
		return
	}
	c.Log = c.Log.With("id", id)

	if handler.composer.UsesLocker {
		lock, err := handler.lockUpload(c, id)
		if err != nil {
			handler.sendError(c, err)
			return
		}

		defer lock.Unlock()
	}

	upload, err := handler.composer.Core.GetUpload(c, id)
	if err != nil {
		handler.sendError(c, err)
		return
	}

	info, err := upload.GetInfo(c)
	if err != nil {
		handler.sendError(c, err)
		return
	}

	// Modifying a final upload is not allowed
	if info.IsFinal {
		handler.sendError(c, models.ErrModifyFinal)
		return
	}

	if offset != info.Offset {
		handler.sendError(c, models.ErrMismatchOffset)
		return
	}

	// TODO: If Upload-Complete: ?1 and Content-Length is set, we can
	// - declare the length already here
	// - validate that the length from this request matches info.Size if !info.SizeIsDeferred

	resp := models.HTTPResponse{
		StatusCode: http.StatusNoContent,
		Header:     make(models.HTTPHeader, 1), // Initialize map, so writeChunk can set the Upload-Offset header.
	}

	// Do not proxy the call to the data store if the upload is already completed
	if !info.SizeIsDeferred && info.Offset == info.Size {
		resp.Header["Upload-Offset"] = strconv.FormatInt(offset, 10)
		handler.sendResp(c, resp)
		return
	}

	if r.Header.Get("Upload-Length") != "" {
		if !handler.composer.UsesLengthDeferrer {
			handler.sendError(c, models.ErrNotImplemented)
			return
		}
		if !info.SizeIsDeferred {
			handler.sendError(c, models.ErrInvalidUploadLength)
			return
		}
		uploadLength, err := strconv.ParseInt(r.Header.Get("Upload-Length"), 10, 64)
		if err != nil || uploadLength < 0 || uploadLength < info.Offset || (handler.config.MaxSize > 0 && uploadLength > handler.config.MaxSize) {
			handler.sendError(c, models.ErrInvalidUploadLength)
			return
		}

		lengthDeclarableUpload := handler.composer.LengthDeferrer.AsLengthDeclarableUpload(upload)
		if err := lengthDeclarableUpload.DeclareLength(c, uploadLength); err != nil {
			handler.sendError(c, err)
			return
		}

		info.Size = uploadLength
		info.SizeIsDeferred = false
	}

	resp, err = handler.writeChunk(c, resp, upload, info)
	if err != nil {
		handler.sendError(c, err)
		return
	}

	isComplete := r.Header.Get("Upload-Complete") == "?1"
	if isComplete && info.SizeIsDeferred {
		info, err = upload.GetInfo(c)
		if err != nil {
			handler.sendError(c, err)
			return
		}

		uploadLength := info.Offset

		lengthDeclarableUpload := handler.composer.LengthDeferrer.AsLengthDeclarableUpload(upload)
		if err := lengthDeclarableUpload.DeclareLength(c, uploadLength); err != nil {
			handler.sendError(c, err)
			return
		}

		info.Size = uploadLength
		info.SizeIsDeferred = false

		resp, err = handler.finishUploadIfComplete(c, resp, upload, info)
		if err != nil {
			handler.sendError(c, err)
			return
		}
	}

	handler.sendResp(c, resp)
}

// writeChunk reads the body from the requests r and appends it to the upload
// with the corresponding id. Afterwards, it will set the necessary response
// headers but will not send the response.
func (handler *UnroutedHandler) writeChunk(c *models.HttpContext, resp models.HTTPResponse, upload models.Upload, info models.FileInfo) (models.HTTPResponse, error) {
	// Get Content-Length if possible
	r := c.GetReq()
	length := r.ContentLength
	offset := info.Offset

	// Test if this upload fits into the file's size
	if !info.SizeIsDeferred && offset+length > info.Size {
		return resp, models.ErrSizeExceeded
	}

	maxSize := info.Size - offset
	// If the upload's length is deferred and the PATCH request does not contain the Content-Length
	// header (which is allowed if 'Transfer-Encoding: chunked' is used), we still need to set limits for
	// the body size.
	if info.SizeIsDeferred {
		if handler.config.MaxSize > 0 {
			// Ensure that the upload does not exceed the maximum upload size
			maxSize = handler.config.MaxSize - offset
		} else {
			// If no upload limit is given, we allow arbitrary sizes
			maxSize = math.MaxInt64
		}
	}
	if length > 0 {
		maxSize = length
	}

	c.Log.Info("ChunkWriteStart", "maxSize", maxSize, "offset", offset)

	var bytesWritten int64
	var err error
	// Prevent a nil pointer dereference when accessing the body which may not be
	// available in the case of a malicious request.
	if r.Body != nil {
		// Limit the data read from the request's body to the allowed maximum. We use
		// http.MaxBytesReader instead of io.LimitedReader because it returns an error
		// if too much data is provided (handled in BodyReader) and also stops the server
		// from reading the remaining request body.
		c.Body = models.NewBodyReader(c, maxSize)
		c.Body.SetOnReadDone(func() {
			// Update the read deadline for every successful read operation. This ensures that the request handler
			// keeps going while data is transmitted but that dead connections can also time out and be cleaned up.
			if err := c.GetResC().SetReadDeadline(time.Now().Add(handler.config.NetworkTimeout)); err != nil {
				c.Log.Warn("NetworkTimeoutError", "error", err)
			}

			// The write deadline is updated accordingly to ensure that we can also write responses.
			if err := c.GetResC().SetWriteDeadline(time.Now().Add(2 * handler.config.NetworkTimeout)); err != nil {
				c.Log.Warn("NetworkTimeoutError", "error", err)
			}
		})

		// We use a callback to allow the hook system to cancel an upload. The callback
		// cancels the request context causing the request body to be closed with the
		// provided error.
		info.SetStopUpload(func(res models.HTTPResponse) {
			cause := models.ErrUploadStoppedByServer
			cause.HTTPResponse = cause.HTTPResponse.MergeWith(res)
			c.GetCancel()(cause)
		})

		if handler.config.NotifyUploadProgress {
			handler.sendProgressMessages(c, info)
		}

		bytesWritten, err = upload.WriteChunk(c, offset, c.Body)

		// If we encountered an error while reading the body from the HTTP request, log it, but only include
		// it in the response, if the store did not also return an error.
		bodyErr := c.Body.HasError()
		if bodyErr != nil {
			c.Log.Error("BodyReaderror", "error", bodyErr.Error())
			if err == nil {
				err = bodyErr
			}
		}

		// Terminate the upload if it was stopped, as indicated by the ErrUploadStoppedByServer error.
		terminateUpload := errors.Is(bodyErr, models.ErrUploadStoppedByServer)
		if terminateUpload && handler.composer.UsesTerminater {
			if terminateErr := handler.terminateUpload(c, upload, info); terminateErr != nil {
				// We only log this error and not show it to the user since this
				// termination error is not relevant to the uploading client
				c.Log.Error("UploadStopTerminateError", "error", terminateErr.Error())
			}
		}
	}

	c.Log.Info("ChunkWriteComplete", "bytesWritten", bytesWritten)

	// Send new offset to client
	newOffset := offset + bytesWritten
	resp.Header["Upload-Offset"] = strconv.FormatInt(newOffset, 10)
	handler.Metrics.IncBytesReceived(uint64(bytesWritten))
	info.Offset = newOffset

	// We try to finish the upload, even if an error occurred. If we have a previous error,
	// we return it and its HTTP response.
	finishResp, finishErr := handler.finishUploadIfComplete(c, resp, upload, info)
	if err != nil {
		return resp, err
	}

	return finishResp, finishErr
}

// finishUploadIfComplete checks whether an upload is completed (i.e. upload offset
// matches upload size) and if so, it will call the data store's FinishUpload
// function and send the necessary message on the CompleteUpload channel.
func (handler *UnroutedHandler) finishUploadIfComplete(c *models.HttpContext, resp models.HTTPResponse, upload models.Upload, info models.FileInfo) (models.HTTPResponse, error) {
	// If the upload is completed, ...
	if !info.SizeIsDeferred && info.Offset == info.Size {
		// ... allow the data storage to finish and cleanup the upload
		if err := upload.FinishUpload(c); err != nil {
			return resp, err
		}

		// ... allow the hook callback to run before sending the response
		if handler.config.PreFinishResponseCallback != nil {
			resp2, err := handler.config.PreFinishResponseCallback(models.NewHookEvent(c, info))
			if err != nil {
				return resp, err
			}
			resp = resp.MergeWith(resp2)
		}

		c.Log.Info("UploadFinished", "size", info.Size)
		handler.Metrics.IncUploadsFinished()

		// ... send the info out to the channel
		if handler.config.NotifyCompleteUploads {
			handler.CompleteUploads <- models.NewHookEvent(c, info)
		}
	}

	return resp, nil
}

// GetFile handles requests to download a file using a GET request. This is not
// part of the specification.
func (handler *UnroutedHandler) GetFile(w http.ResponseWriter, r *http.Request) {
	bucketName := r.Header.Get("bucket-name")
	endpoint := r.Header.Get("endpoint")
	s3c := handler.config.Service
	if endpoint != "" {
		s3c = s3.New(s3.Options{
			Region: "",
			Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
				handler.config.S3Key,
				handler.config.S3Secret,
				"")),
			BaseEndpoint: &endpoint,
			UsePathStyle: true,
		})
	}

	if bucketName != "" {
		store := s3store.New(bucketName, s3c)
		composer := models.NewStoreComposer()
		store.UseIn(composer)
		handler.composer = composer
	}

	c := handler.getContext(w, r)

	id, err := extractIDFromPath(r.URL.Path)
	if err != nil {
		handler.sendError(c, err)
		return
	}
	c.Log = c.Log.With("id", id)

	if handler.composer.UsesLocker {
		lock, err := handler.lockUpload(c, id)
		if err != nil {
			handler.sendError(c, err)
			return
		}

		defer lock.Unlock()
	}

	upload, err := handler.composer.Core.GetUpload(c, id)
	if err != nil {
		handler.sendError(c, err)
		return
	}

	info, err := upload.GetInfo(c)
	if err != nil {
		handler.sendError(c, err)
		return
	}

	contentType, contentDisposition := filterContentType(info)
	resp := models.HTTPResponse{
		StatusCode: http.StatusOK,
		Header: models.HTTPHeader{
			"Content-Length":      strconv.FormatInt(info.Offset, 10),
			"Content-Type":        contentType,
			"Content-Disposition": contentDisposition,
		},
		Body: "", // Body is intentionally left empty, and we copy it manually in later.
	}

	// If no data has been uploaded yet, respond with an empty "204 No Content" status.
	if info.Offset == 0 {
		resp.StatusCode = http.StatusNoContent
		handler.sendResp(c, resp)
		return
	}

	src, err := upload.GetReader(c)
	if err != nil {
		handler.sendError(c, err)
		return
	}

	handler.sendResp(c, resp)
	io.Copy(w, src)

	src.Close()
}

// mimeInlineBrowserWhitelist is a map containing MIME types which should be
// allowed to be rendered by browser inline, instead of being forced to be
// downloaded. For example, HTML or SVG files are not allowed, since they may
// contain malicious JavaScript. In a similiar fashion PDF is not on this list
// as their parsers commonly contain vulnerabilities which can be exploited.
// The values of this map does not convey any meaning and are therefore just
// empty structs.
var mimeInlineBrowserWhitelist = map[string]struct{}{
	"text/plain": {},

	"image/png":  {},
	"image/jpeg": {},
	"image/gif":  {},
	"image/bmp":  {},
	"image/webp": {},

	"audio/wave":      {},
	"audio/wav":       {},
	"audio/x-wav":     {},
	"audio/x-pn-wav":  {},
	"audio/webm":      {},
	"video/webm":      {},
	"audio/ogg":       {},
	"video/ogg":       {},
	"application/ogg": {},
}

// filterContentType returns the values for the Content-Type and
// Content-Disposition headers for a given upload. These values should be used
// in responses for GET requests to ensure that only non-malicious file types
// are shown directly in the browser. It will extract the file name and type
// from the "fileame" and "filetype".
// See https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Content-Disposition
func filterContentType(info models.FileInfo) (contentType string, contentDisposition string) {
	filetype := info.MetaData["filetype"]

	if models.ReMimeType.MatchString(filetype) {
		// If the filetype from metadata is well formed, we forward use this
		// for the Content-Type header. However, only whitelisted mime types
		// will be allowed to be shown inline in the browser
		contentType = filetype
		if _, isWhitelisted := mimeInlineBrowserWhitelist[filetype]; isWhitelisted {
			contentDisposition = "inline"
		} else {
			contentDisposition = "attachment"
		}
	} else {
		// If the filetype from the metadata is not well formed, we use a
		// default type and force the browser to download the content.
		contentType = "application/octet-stream"
		contentDisposition = "attachment"
	}

	// Add a filename to Content-Disposition if one is available in the metadata
	if filename, ok := info.MetaData["filename"]; ok {
		contentDisposition += ";filename=" + strconv.Quote(filename)
	}

	return contentType, contentDisposition
}

// DelFile terminates an upload permanently.
func (handler *UnroutedHandler) DelFile(w http.ResponseWriter, r *http.Request) {
	bucketName := r.Header.Get("bucket-name")
	endpoint := r.Header.Get("endpoint")
	s3c := handler.config.Service
	if endpoint != "" {
		s3c = s3.New(s3.Options{
			Region: "",
			Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
				handler.config.S3Key,
				handler.config.S3Secret,
				"")),
			BaseEndpoint: &endpoint,
			UsePathStyle: true,
		})
	}

	if bucketName != "" {
		store := s3store.New(bucketName, s3c)
		composer := models.NewStoreComposer()
		store.UseIn(composer)
		handler.composer = composer
	}

	c := handler.getContext(w, r)

	// Abort the request handling if the required interface is not implemented
	if !handler.composer.UsesTerminater {
		handler.sendError(c, models.ErrNotImplemented)
		return
	}

	id, err := extractIDFromPath(r.URL.Path)
	if err != nil {
		handler.sendError(c, err)
		return
	}
	c.Log = c.Log.With("id", id)

	if handler.composer.UsesLocker {
		lock, err := handler.lockUpload(c, id)
		if err != nil {
			handler.sendError(c, err)
			return
		}

		defer lock.Unlock()
	}

	upload, err := handler.composer.Core.GetUpload(c, id)
	if err != nil {
		handler.sendError(c, err)
		return
	}

	var info models.FileInfo
	if handler.config.NotifyTerminatedUploads {
		info, err = upload.GetInfo(c)
		if err != nil {
			handler.sendError(c, err)
			return
		}
	}

	err = handler.terminateUpload(c, upload, info)
	if err != nil {
		handler.sendError(c, err)
		return
	}

	handler.sendResp(c, models.HTTPResponse{
		StatusCode: http.StatusNoContent,
	})
}

// terminateUpload passes a given upload to the DataStore's Terminater,
// send the corresponding upload info on the TerminatedUploads channnel
// and updates the statistics.
// Note the the info argument is only needed if the terminated uploads
// notifications are enabled.
func (handler *UnroutedHandler) terminateUpload(c *models.HttpContext, upload models.Upload, info models.FileInfo) error {
	terminatableUpload := handler.composer.Terminater.AsTerminatableUpload(upload)

	err := terminatableUpload.Terminate(c)
	if err != nil {
		return err
	}

	if handler.config.NotifyTerminatedUploads {
		handler.TerminatedUploads <- models.NewHookEvent(c, info)
	}

	c.Log.Info("UploadTerminated")
	handler.Metrics.IncUploadsTerminated()

	return nil
}

// Send the error in the response body. The status code will be looked up in
// ErrStatusCodes. If none is found 500 Internal Error will be used.
func (handler *UnroutedHandler) sendError(c *models.HttpContext, err error) {
	r := c.GetReq()

	detailedErr, ok := err.(models.Error)
	if !ok {
		c.Log.Error("InternalServerError", "message", err.Error())
		detailedErr = models.NewError("ERR_INTERNAL_SERVER_ERROR", err.Error(), http.StatusInternalServerError)
	}

	// If we are sending the response for a HEAD request, ensure that we are not including
	// any response body.
	if r.Method == "HEAD" {
		detailedErr.HTTPResponse.Body = ""
	}

	handler.sendResp(c, detailedErr.HTTPResponse)
	handler.Metrics.IncErrorsTotal(detailedErr)
}

// sendResp writes the header to w with the specified status code.
func (handler *UnroutedHandler) sendResp(c *models.HttpContext, resp models.HTTPResponse) {
	resp.WriteTo(c.GetRes())

	c.Log.Info("ResponseOutgoing", "status", resp.StatusCode, "body", resp.Body)
}

// Make an absolute URLs to the given upload id. If the base path is absolute
// it will be prepended else the host and protocol from the request is used.
func (handler *UnroutedHandler) absFileURL(r *http.Request, id string) string {
	if handler.isBasePathAbs {
		return handler.basePath + id
	}

	// Read origin and protocol from request
	host, proto := getHostAndProtocol(r, handler.config.RespectForwardedHeaders)

	url := proto + "://" + host + handler.basePath + id

	return url
}

// sendProgressMessage will send a notification over the UploadProgress channel
// indicating how much data has been transfered to the server.
// It will stop sending these instances once the provided context is done.
func (handler *UnroutedHandler) sendProgressMessages(c *models.HttpContext, info models.FileInfo) {
	hook := models.NewHookEvent(c, info)

	previousOffset := int64(0)
	originalOffset := hook.Upload.Offset

	emitProgress := func() {
		hook.Upload.Offset = originalOffset + c.Body.BytesRead()
		if hook.Upload.Offset != previousOffset {
			handler.UploadProgress <- hook
			previousOffset = hook.Upload.Offset
		}
	}

	go func() {
		for {
			select {
			case <-c.Done():
				emitProgress()
				return
			case <-time.After(handler.config.UploadProgressInterval):
				emitProgress()
			}
		}
	}()
}

// getHostAndProtocol extracts the host and used protocol (either HTTP or HTTPS)
// from the given request. If `allowForwarded` is set, the X-Forwarded-Host,
// X-Forwarded-Proto and Forwarded headers will also be checked to
// support proxies.
func getHostAndProtocol(r *http.Request, allowForwarded bool) (host, proto string) {
	if r.TLS != nil {
		proto = "https"
	} else {
		proto = "http"
	}

	host = r.Host

	if !allowForwarded {
		return
	}

	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}

	if h := r.Header.Get("X-Forwarded-Proto"); h == "http" || h == "https" {
		proto = h
	}

	if h := r.Header.Get("Forwarded"); h != "" {
		if r := models.ReForwardedHost.FindStringSubmatch(h); len(r) == 2 {
			host = r[1]
		}

		if r := models.ReForwardedProto.FindStringSubmatch(h); len(r) == 2 {
			proto = r[1]
		}
	}

	return
}

// The get sum of all sizes for a list of upload ids while checking whether
// all of these uploads are finished yet. This is used to calculate the size
// of a final resource.
func (handler *UnroutedHandler) sizeOfUploads(ctx context.Context, ids []string) (partialUploads []models.Upload, size int64, err error) {
	partialUploads = make([]models.Upload, len(ids))

	for i, id := range ids {
		upload, err := handler.composer.Core.GetUpload(ctx, id)
		if err != nil {
			return nil, 0, err
		}

		info, err := upload.GetInfo(ctx)
		if err != nil {
			return nil, 0, err
		}

		if info.SizeIsDeferred || info.Offset != info.Size {
			err = models.ErrUploadNotFinished
			return nil, 0, err
		}

		size += info.Size
		partialUploads[i] = upload
	}

	return
}

// Verify that the Upload-Length and Upload-Defer-Length headers are acceptable for creating a
// new upload
func (handler *UnroutedHandler) validateNewUploadLengthHeaders(uploadLengthHeader string, uploadDeferLengthHeader string) (uploadLength int64, uploadLengthDeferred bool, err error) {
	haveBothLengthHeaders := uploadLengthHeader != "" && uploadDeferLengthHeader != ""
	haveInvalidDeferHeader := uploadDeferLengthHeader != "" && uploadDeferLengthHeader != models.UploadLengthDeferred
	lengthIsDeferred := uploadDeferLengthHeader == models.UploadLengthDeferred

	if lengthIsDeferred && !handler.composer.UsesLengthDeferrer {
		err = models.ErrNotImplemented
	} else if haveBothLengthHeaders {
		err = models.ErrUploadLengthAndUploadDeferLength
	} else if haveInvalidDeferHeader {
		err = models.ErrInvalidUploadDeferLength
	} else if lengthIsDeferred {
		uploadLengthDeferred = true
	} else {
		uploadLength, err = strconv.ParseInt(uploadLengthHeader, 10, 64)
		if err != nil || uploadLength < 0 {
			err = models.ErrInvalidUploadLength
		}
	}

	return
}

// lockUpload creates a new lock for the given upload ID and attempts to lock it.
// The created lock is returned if it was aquired successfully.
func (handler *UnroutedHandler) lockUpload(c *models.HttpContext, id string) (models.Lock, error) {
	lock, err := handler.composer.Locker.NewLock(id)
	if err != nil {
		return nil, err
	}

	ctx, cancelContext := context.WithTimeout(c, handler.config.AcquireLockTimeout)
	defer cancelContext()

	// No need to wrap this in a sync.OnceFunc because c.cancel will be a noop after the first call.
	releaseLock := func() {
		c.Log.Info("UploadInterrupted")
		c.GetCancel()(models.ErrUploadInterrupted)
	}

	if err := lock.Lock(ctx, releaseLock); err != nil {
		return nil, err
	}

	return lock, nil
}

// isResumableUploadDraftRequest returns whether a HTTP request includes a sign that it is
// related to resumable upload draft from IETF (instead of tus v1)
func (handler UnroutedHandler) isResumableUploadDraftRequest(r *http.Request) bool {
	return handler.config.EnableExperimentalProtocol && r.Header.Get("Upload-Draft-Interop-Version") == models.CurrentUploadDraftInteropVersion
}

// newContext constructs a new httpContext for the given request. This should only be done once
// per request and the context should be stored in the request, so it can be fetched with getContext.
func (h UnroutedHandler) newContext(w http.ResponseWriter, r *http.Request) *models.HttpContext {
	// requestCtx is the context from the native request instance. It gets cancelled
	// if the connection closes, the request is cancelled (HTTP/2), ServeHTTP returns
	// or the server's base context is cancelled.
	requestCtx := r.Context()
	// On top of requestCtx, we construct a context that we can cancel, for example when
	// the post-receive hook stops an upload or if another uploads requests a lock to be released.
	cancellableCtx, cancelHandling := context.WithCancelCause(requestCtx)
	// On top of cancellableCtx, we construct a new context which gets cancelled with a delay.
	// See HookEvent.Context for more details, but the gist is that we want to give data stores
	// some more time to finish their buisness.
	delayedCtx := models.NewDelayedContext(cancellableCtx, h.config.GracefulRequestCompletionTimeout)

	ctx := models.NewHttpContext(delayedCtx, r, w, http.NewResponseController(w), cancelHandling, h.logger.With("method", r.Method, "path", r.URL.Path, "requestId", getRequestId(r)))

	go func() {
		<-cancellableCtx.Done()

		// If the cause is one of our own errors, close a potential body and relay the error.
		cause := context.Cause(cancellableCtx)
		if (errors.Is(cause, models.ErrServerShutdown) || errors.Is(cause, models.ErrUploadInterrupted) || errors.Is(cause, models.ErrUploadStoppedByServer)) && ctx.Body != nil {
			ctx.Body.CloseWithError(cause)
		}
	}()

	return ctx
}

// getContext tries to retrieve a httpContext from the request or constructs a new one.
func (h UnroutedHandler) getContext(w http.ResponseWriter, r *http.Request) *models.HttpContext {
	c, ok := r.Context().(*models.HttpContext)
	if !ok {
		c = h.newContext(w, r)
	}

	return c
}

// ParseMetadataHeader parses the Upload-Metadata header as defined in the
// File Creation extension.
// e.g. Upload-Metadata: name bHVucmpzLnBuZw==,type aW1hZ2UvcG5n
func ParseMetadataHeader(header string) map[string]string {
	meta := make(map[string]string)

	for _, element := range strings.Split(header, ",") {
		element := strings.TrimSpace(element)

		parts := strings.Split(element, " ")

		if len(parts) > 2 {
			continue
		}

		key := parts[0]
		if key == "" {
			continue
		}

		value := ""
		if len(parts) == 2 {
			// Ignore current element if the value is no valid base64
			dec, err := base64.StdEncoding.DecodeString(parts[1])
			if err != nil {
				continue
			}

			value = string(dec)
		}

		meta[key] = value
	}

	return meta
}

// SerializeMetadataHeader serializes a map of strings into the Upload-Metadata
// header format used in the response for HEAD requests.
// e.g. Upload-Metadata: name bHVucmpzLnBuZw==,type aW1hZ2UvcG5n
func SerializeMetadataHeader(meta map[string]string) string {
	header := ""
	for key, value := range meta {
		valueBase64 := base64.StdEncoding.EncodeToString([]byte(value))
		header += key + " " + valueBase64 + ","
	}

	// Remove trailing comma
	if len(header) > 0 {
		header = header[:len(header)-1]
	}

	return header
}

// Parse the Upload-Concat header, e.g.
// Upload-Concat: partial
// Upload-Concat: final;http://tus.io/files/a /files/b/
func parseConcat(header string) (isPartial bool, isFinal bool, partialUploads []string, err error) {
	if len(header) == 0 {
		return
	}

	if header == "partial" {
		isPartial = true
		return
	}

	l := len("final;")
	if strings.HasPrefix(header, "final;") && len(header) > l {
		isFinal = true

		list := strings.Split(header[l:], " ")
		for _, value := range list {
			value := strings.TrimSpace(value)
			if value == "" {
				continue
			}

			id, extractErr := extractIDFromPath(value)
			if extractErr != nil {
				err = extractErr
				return
			}

			partialUploads = append(partialUploads, id)
		}
	}

	// If no valid partial upload ids are extracted this is not a final upload.
	if len(partialUploads) == 0 {
		isFinal = false
		err = models.ErrInvalidConcat
	}

	return
}

// extractIDFromPath pulls the last segment from the url provided
func extractIDFromPath(url string) (string, error) {
	result := models.ReExtractFileID.FindStringSubmatch(url)
	if len(result) != 2 {
		return "", models.ErrNotFound
	}
	return result[1], nil
}

// getRequestId returns the value of the X-Request-ID header, if available,
// and also takes care of truncating the input.
func getRequestId(r *http.Request) string {
	reqId := r.Header.Get("X-Request-ID")
	if reqId == "" {
		return ""
	}

	// Limit the length of the request ID to 36 characters, which is enough
	// to fit a UUID.
	if len(reqId) > 36 {
		reqId = reqId[:36]
	}

	return reqId
}
