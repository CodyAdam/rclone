// multipart upload for box

package dynbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/rclone/rclone/backend/dynbox/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/lib/atexit"
	"github.com/rclone/rclone/lib/rest"
)

const (
	GB = 1024 * 1024 * 1024
	MB = 1024 * 1024
)

// getChunkSize returns the appropriate chunk size based on file size
func getChunkSize(size int64) int64 {
	switch {
	case size == 0:
		return 50 * MB
	case size > 100*GB:
		return 400 * MB
	case size > 1*GB:
		return 100 * MB
	default:
		return 50 * MB
	}
}

// uploadMultipart uploads a file using multipart upload
func (o *Object) uploadMultipart(ctx context.Context, in io.Reader, leaf, directoryID string, size int64, contentType string, modTime time.Time, hash string, options ...fs.OpenOption) (err error) {

	// Create upload session
	session, err := o.createUploadSession(ctx, leaf, directoryID, size, contentType, modTime, hash)
	if err != nil {
		return fmt.Errorf("multipart upload create session failed: %w", err)
	}
	if session.MultipartUploadId == nil {
		// No upload needed - file was cached
		return o.readMetaData(ctx)
	}

	chunkSize := getChunkSize(size)
	totalParts := (size + chunkSize - 1) / chunkSize // Round up division
	fs.Debugf(o, "Multipart upload session started for %d parts of size %v", totalParts, fs.SizeSuffix(chunkSize))

	// Cancel the session if something went wrong
	defer atexit.OnError(&err, func() {
		fs.Debugf(o, "Cancelling multipart upload: %v", err)
		cancelErr := o.abortUpload(ctx, *session.MultipartUploadId, session.Key)
		if cancelErr != nil {
			fs.Logf(o, "Failed to cancel multipart upload: %v", cancelErr)
		}
	})()

	// unwrap the accounting from the input, we use wrap to put it
	// back on after the buffering
	in, wrap := accounting.UnWrap(in)

	// Upload the chunks
	remaining := size
	position := int64(0)
	parts := make([]api.Part, totalParts)
	verifHash := sha256.New()
	errs := make(chan error, totalParts)
	var wg sync.WaitGroup
outer:
	for part := int64(0); part < totalParts; part++ {
		// Check any errors
		select {
		case err = <-errs:
			break outer
		default:
		}

		reqSize := remaining
		if reqSize >= chunkSize {
			reqSize = chunkSize
		}

		// Make a block of memory
		buf := make([]byte, reqSize)

		// Read the chunk
		_, err = io.ReadFull(in, buf)
		if err != nil {
			err = fmt.Errorf("multipart upload failed to read source: %w", err)
			break outer
		}

		// Make the global verifHash (must be done sequentially)
		_, _ = verifHash.Write(buf)

		// Transfer the chunk
		wg.Add(1)
		go func(part int64, position int64) {
			defer wg.Done()
			fs.Debugf(o, "Uploading part %d/%d offset %v/%v part size %v", part+1, totalParts, fs.SizeSuffix(position), fs.SizeSuffix(size), fs.SizeSuffix(chunkSize))
			partResponse, err := o.uploadPart(ctx, session, part+1, buf, wrap, options...)
			if err != nil {
				err = fmt.Errorf("multipart upload failed to upload part: %w", err)
				select {
				case errs <- err:
				default:
				}
				return
			}
			parts[part] = partResponse
		}(part, position)

		// ready for next block
		remaining -= chunkSize
		position += chunkSize
	}
	wg.Wait()
	if err == nil {
		select {
		case err = <-errs:
		default:
		}
	}
	if err != nil {
		return err
	}

	// Verify the hash matches
	calculatedHash := fmt.Sprintf("%x", verifHash.Sum(nil))
	if calculatedHash != hash {
		return fmt.Errorf("multipart upload failed hash verification: expected %s got %s", hash, calculatedHash)
	}

	// Finalise the upload session
	err = o.commitUpload(ctx, session, parts)
	if err != nil {
		return fmt.Errorf("multipart upload failed to finalize: %w", err)
	}

	return o.finishUpload(ctx, session.Key)
}

// createUploadSession creates an upload session for the object
func (o *Object) createUploadSession(ctx context.Context, leaf string, directoryID string, size int64, contentType string, modTime time.Time, hash string) (uploadResp *api.UploadMultipartRequestResponse, err error) {

	// Create new file
	uploadReq := api.RequestUploadCreate{
		Name:     o.fs.opt.Enc.FromStandardName(leaf),
		Size:     size,
		Type:     contentType,
		VaultID:  o.fs.opt.VaultID,
		Hash:     hash,
		ModTime:  api.Time(modTime),
		ParentID: &directoryID,
	}
	opts := rest.Opts{
		Method: "POST",
		Path:   "/fs/files/upload/multipart",
	}

	var resp *http.Response
	err = o.fs.pacer.CallNoRetry(func() (bool, error) {
		resp, err = o.fs.srv.CallJSON(ctx, &opts, uploadReq, &uploadResp)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return nil, err
	}

	return uploadResp, nil
}

// uploadPart uploads a part in an upload session
func (o *Object) uploadPart(ctx context.Context, session *api.UploadMultipartRequestResponse, partNumber int64, chunk []byte, wrap accounting.WrapFn, options ...fs.OpenOption) (response api.Part, err error) {
	// Get presigned URL for this part
	opts := rest.Opts{
		Method:  "PUT",
		Path:    "/fs/files/upload/multipart/" + *session.MultipartUploadId,
		Options: options,
	}

	chunkSize := int64(len(chunk))

	signRequest := api.SignPartRequest{
		Key: session.Key,
		Parts: []struct {
			PartNumber int64 `json:"PartNumber"`
			Size       int64 `json:"Size"`
		}{{PartNumber: partNumber, Size: chunkSize}},
	}

	var signResponse api.SignPartResponse
	err = o.fs.pacer.Call(func() (bool, error) {
		resp, err := o.fs.srv.CallJSON(ctx, &opts, &signRequest, &signResponse)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return response, fmt.Errorf("failed to get presigned URL: %w", err)
	}

	if len(signResponse) == 0 {
		return response, errors.New("no presigned URL returned")
	}

	// Upload the part using the presigned URL
	presignedURL := signResponse[0].UploadUrl
	req, err := http.NewRequestWithContext(ctx, "PUT", presignedURL, wrap(bytes.NewReader(chunk)))
	if err != nil {
		return response, fmt.Errorf("failed to create request: %w", err)
	}
	req.ContentLength = chunkSize

	client := http.DefaultClient
	resp, err := client.Do(req)
	if err != nil {
		return response, fmt.Errorf("failed to upload part: %w", err)
	}
	defer fs.CheckClose(resp.Body, &err)

	if resp.StatusCode != http.StatusOK {
		return response, fmt.Errorf("failed to upload part: %s", resp.Status)
	}

	// Get ETag from response
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return response, errors.New("no ETag returned from upload")
	}

	response = api.Part{
		PartNumber: partNumber,
		Size:       chunkSize,
		ETag:       etag,
	}

	return response, nil
}

// commitUpload finishes an upload session
func (o *Object) commitUpload(ctx context.Context, session *api.UploadMultipartRequestResponse, parts []api.Part) (err error) {
	opts := rest.Opts{
		Method: "POST",
		Path:   "/fs/files/upload/multipart/" + *session.MultipartUploadId,
	}

	request := api.CompleteMultipartUpload{
		Key:   session.Key,
		Parts: parts,
	}

	var response api.CompleteMultipartUploadResponse
	err = o.fs.pacer.Call(func() (bool, error) {
		resp, err := o.fs.srv.CallJSON(ctx, &opts, &request, &response)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return fmt.Errorf("failed to complete multipart upload: %w", err)
	}
	return nil
}

// abortUpload cancels an upload session
func (o *Object) abortUpload(ctx context.Context, SessionID string, Key string) (err error) {
	opts := rest.Opts{
		Method:     "DELETE",
		Path:       "/fs/files/upload/multipart/" + SessionID,
		NoResponse: true,
	}
	input := api.UploadAbortRequest{
		Key: Key,
	}
	var resp *http.Response
	err = o.fs.pacer.Call(func() (bool, error) {
		resp, err = o.fs.srv.CallJSON(ctx, &opts, &input, nil)
		return shouldRetry(ctx, resp, err)
	})
	return err
}
