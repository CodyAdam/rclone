// Package api has type definitions for dynbox
package api

import (
	"fmt"
	"time"
)

const (
	// 2017-05-03T07:26:10-07:00
	timeFormat = `"` + time.RFC3339 + `"`
)

// Time represents date and time information for the
// box API, by using RFC3339
type Time time.Time

// MarshalJSON turns a Time into JSON as Unix epoch milliseconds
func (t *Time) MarshalJSON() (out []byte, err error) {
	epochMs := (*time.Time)(t).UnixNano() / int64(time.Millisecond)
	return []byte(fmt.Sprintf("%d", epochMs)), nil
}

// UnmarshalJSON turns JSON into a Time
func (t *Time) UnmarshalJSON(data []byte) error {
	newT, err := time.Parse(timeFormat, string(data))
	if err != nil {
		return err
	}
	*t = Time(newT)
	return nil
}

// Error is returned from box when things go wrong
type Error struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error returns a string for the error and satisfies the error interface
func (e *Error) Error() string {
	out := fmt.Sprintf("Error %q (%d)", e.Code, e.Status)
	if e.Message != "" {
		out += ": " + e.Message
	}
	return out
}

// Check Error satisfies the error interface
var _ error = (*Error)(nil)

// Item describes a folder or a file as returned by Get Folder Items and others
type Item struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	VaultID   string  `json:"vaultId"`
	UpdatedAt Time    `json:"updatedAt"`
	UpdatedBy *string `json:"updatedBy,omitempty"`
	CreatedAt Time    `json:"createdAt"`
	DeletedAt *Time   `json:"deletedAt,omitempty"`
}

type FileItem struct {
	Item
	Size             int64   `json:"size"`
	FolderPath       string  `json:"folderPath"`
	CurrentVersionID *string `json:"currentVersionId,omitempty"`
	ContentType      string  `json:"type"`
	Path             string  `json:"path"`
	Hash             string  `json:"hash"`
	Key              string  `json:"key"`
}

type FolderItem struct {
	Item
	ParentID *string `json:"parentId,omitempty"` // null if root
}

// ModTime returns the modification time of the item
func (i *Item) ModTime() (t time.Time) {
	t = time.Time(i.UpdatedAt)
	if t.IsZero() {
		t = time.Time(i.CreatedAt)
	}
	return t
}

// IsDeleted returns true if the item is deleted
func (i *Item) IsDeleted() bool {
	return i.DeletedAt != nil
}

type Vault struct {
	ID string `json:"id"`
}

// Usage is returned from Get Usage
type Usage struct {
	Vault struct {
		Plan              string    `json:"plan"`
		MemberCount       int64     `json:"memberCount"`
		TagCount          int64     `json:"tagCount"`
		ViewCount         int64     `json:"viewCount"`
		BillingCycleStart time.Time `json:"billingCycleStart"`
	} `json:"vault"`
	AI struct {
		Used  int64 `json:"used"`
		Total int64 `json:"total"`
		Free  int64 `json:"free"`
	} `json:"ai"`
	FS struct {
		Used         int64 `json:"used"`
		Total        int64 `json:"total"`
		Free         int64 `json:"free"`
		Trashed      int64 `json:"trashed"`
		Other        int64 `json:"other"`
		FolderCount  int64 `json:"folderCount"`
		FileCount    int64 `json:"fileCount"`
		ObjectCount  int64 `json:"objectCount"`
		VersionCount int64 `json:"versionCount"`
	} `json:"fs"`
}

// CreateFolder is the request for Create Folder (return a Item)
type CreateFolder struct {
	ParentID *string `json:"parentId,omitempty"`
	Name     string  `json:"name"`
	VaultID  string  `json:"vaultId"`
}

type GetMetadataFromPath struct {
	FilePath string `json:"filePath"`
	VaultID  string `json:"vaultId"`
}

type GetFolderItems struct {
	FolderID string `json:"folderId"`
	VaultID  string `json:"vaultId"`
}

// FolderItems is returned from the listAll call
type FolderItems struct {
	Files   []FileItem   `json:"files"`
	Folders []FolderItem `json:"folders"`
}

// PreUploadCheck is the request for upload preflight check
type PreUploadCheck struct {
	FolderID *string `json:"folderId,omitempty"`
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	VaultID  string  `json:"vaultId"`
}

// PurgeCheck is the request for Purge Check
type PurgeCheck struct {
	FolderID  *string `json:"folderId"`
	VaultID   string  `json:"vaultId"`
	Permanent bool    `json:"permanent,omitempty"`
	Recursive bool    `json:"recursive,omitempty"`
}

// CopyFile is the request for Copy File
type CopyFile struct {
	NewParentID *string `json:"newParentId,omitempty"`
	NewName     string  `json:"newName"`
}

// Move is the request for Move File or Folder
type Move struct {
	VaultID     string  `json:"vaultId"`
	NewParentID *string `json:"newParentId,omitempty"`
	NewName     string  `json:"newName"`
}

// UpdateFileMetadata is used in Update File Info
type UpdateFileMetadata struct {
	UpdatedAt Time `json:"updatedAt"`
}

type RequestUploadUpdate struct {
	Size      int64  `json:"size"`
	Type      string `json:"type"`
	Hash      string `json:"hash"`
	UpdatedAt *Time  `json:"updatedAt,omitempty"`
}

type RequestUploadCreate struct {
	Name      string  `json:"name"`
	Size      int64   `json:"size"`
	Type      string  `json:"type"`
	VaultID   string  `json:"vaultId"`
	Hash      string  `json:"hash"`
	CreatedAt *Time   `json:"createdAt,omitempty"`
	ParentID  *string `json:"parentId,omitempty"` // null if root
}

type UploadRequestResponse struct {
	UploadUrl string `json:"uploadUrl"`
}

// TODO TO UPDATE: -----------------------------

// Parent defined the ID of the parent directory
type Parent struct {
	ID string `json:"id"`
}

// UploadFile is the request for Upload File
type UploadFile struct {
	Name              string `json:"name"`
	Parent            Parent `json:"parent"`
	ContentCreatedAt  Time   `json:"content_created_at"`
	ContentModifiedAt Time   `json:"content_modified_at"`
}

// UpdateFileMove is the request for Upload File to change name and parent
type UpdateFileMove struct {
	Name   string `json:"name"`
	Parent Parent `json:"parent"`
}

// UploadSessionRequest is uses in Create Upload Session
type UploadSessionRequest struct {
	FolderID string `json:"folder_id,omitempty"` // don't pass for update
	FileSize int64  `json:"file_size"`
	FileName string `json:"file_name,omitempty"` // optional for update
}

// UploadSessionResponse is returned from Create Upload Session
type UploadSessionResponse struct {
	TotalParts       int   `json:"total_parts"`
	PartSize         int64 `json:"part_size"`
	SessionEndpoints struct {
		ListParts  string `json:"list_parts"`
		Commit     string `json:"commit"`
		UploadPart string `json:"upload_part"`
		Status     string `json:"status"`
		Abort      string `json:"abort"`
	} `json:"session_endpoints"`
	SessionExpiresAt  Time   `json:"session_expires_at"`
	ID                string `json:"id"`
	Type              string `json:"type"`
	NumPartsProcessed int    `json:"num_parts_processed"`
}

// Part defines the return from upload part call which are passed to commit upload also
type Part struct {
	PartID string `json:"part_id"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
	Sha1   string `json:"sha1"`
}

// UploadPartResponse is returned from the upload part call
type UploadPartResponse struct {
	Part Part `json:"part"`
}

// FileTreeChangeEventTypes are the events that can require cache invalidation
var FileTreeChangeEventTypes = map[string]struct{}{
	"ITEM_COPY":                 {},
	"ITEM_CREATE":               {},
	"ITEM_MAKE_CURRENT_VERSION": {},
	"ITEM_MODIFY":               {},
	"ITEM_MOVE":                 {},
	"ITEM_RENAME":               {},
	"ITEM_TRASH":                {},
	"ITEM_UNDELETE_VIA_TRASH":   {},
	"ITEM_UPLOAD":               {},
}

// Event is an array element in the response returned from /events
type Event struct {
	EventType string `json:"event_type"`
	EventID   string `json:"event_id"`
	Source    Item   `json:"source"`
}

// Events is returned from /events
type Events struct {
	ChunkSize          int64   `json:"chunk_size"`
	Entries            []Event `json:"entries"`
	NextStreamPosition int64   `json:"next_stream_position"`
}
