// Package api has type definitions for dynbox
package api

import (
	"fmt"
	"time"
)

const (
	// 2025-01-13T13:04:21.939Z
	timeFormat = `"` + time.RFC3339 + `"`
)

// Time represents date and time information for the
// box API, by using RFC3339
type Time time.Time

// MarshalJSON turns a Time into JSON as RFC3339
func (t Time) MarshalJSON() (out []byte, err error) {
	timeString := time.Time(t).Format(timeFormat)
	return []byte(timeString), nil
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

// Error is returned from dynbox when things go wrong
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"status"`
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
	RawModTime       Time    `json:"modTime"`
}

type FolderItem struct {
	Item
	ParentID *string `json:"parentId,omitempty"` // null if root
}

type RequestUploadFinished struct {
	Key string `json:"key"`
}

// IsDeleted returns true if the item is deleted
func (i *Item) IsDeleted() bool {
	return i.DeletedAt != nil
}

func (i *FileItem) ModTime() (t time.Time) {
	return time.Time(i.RawModTime)
}

func (i *Item) UpdateTime() (t time.Time) {
	return time.Time(i.UpdatedAt)
}

type Vault struct {
	ID string `json:"id"`
}

// Usage is returned from Get Usage
type Usage struct {
	Vault struct {
		Plan              string `json:"plan"`
		MemberCount       int64  `json:"memberCount"`
		TagCount          int64  `json:"tagCount"`
		ViewCount         int64  `json:"viewCount"`
		BillingCycleStart int64  `json:"billingCycleStart"`
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

type PreUploadCheckResponse struct {
	CanUpload      bool    `json:"canUpload"`
	ExistingFileID *string `json:"existingFileId,omitempty"`
}

type DeleteFile struct {
	Permanent bool `json:"permanent"`
}

// PurgeCheck is the request for Purge Check
type PurgeCheck struct {
	FolderID  *string `json:"folderId"`
	VaultID   string  `json:"vaultId"`
	Permanent bool    `json:"permanent"`
	Recursive bool    `json:"recursive"`
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
	ModTime Time `json:"modTime"`
}

// --- single upload ---

type RequestUploadCreate struct {
	Name      string  `json:"name"`
	Size      int64   `json:"size"`
	Type      string  `json:"type"`
	VaultID   string  `json:"vaultId"`
	Hash      string  `json:"hash"`
	ModTime   Time    `json:"modTime"`
	CreatedAt *Time   `json:"createdAt,omitempty"`
	ParentID  *string `json:"parentId,omitempty"` // null if root
}

type UploadRequestResponse struct {
	UploadUrl *string `json:"uploadUrl,omitempty"`
	Key       string  `json:"key"`
}

// --- multipart upload ---
type UploadMultipartRequestResponse struct {
	MultipartUploadId *string `json:"multipartUploadId,omitempty"`
	Key               string  `json:"key"`
}
type UploadAbortRequest struct {
	Key string `json:"key"`
}

type ListPartsRequest struct {
	Key string `json:"key"`
}

type ListPartsResponse []Part

type Part struct {
	PartNumber int64  `json:"PartNumber"`
	ETag       string `json:"ETag,omitempty"`
	Size       int64  `json:"Size,omitempty"`
}

type PartWithUrl struct {
	Part
	UploadUrl string `json:"uploadUrl"`
}

type SignPartRequest struct {
	Key   string `json:"key"`
	Parts []struct {
		PartNumber int64 `json:"PartNumber"`
		Size       int64 `json:"Size"`
	} `json:"parts"`
}

type SignPartResponse []PartWithUrl

type CompleteMultipartUpload struct {
	Parts []Part `json:"parts"`
	Key   string `json:"key"`
}

type CompleteMultipartUploadResponse struct {
	Location string `json:"location"`
}

// --- events ---

// EventType represents the type of event that occurred
type EventType string

const (
	EventTypeFileCreated           EventType = "file.created"
	EventTypeFileCopied            EventType = "file.copied"
	EventTypeFileUpdated           EventType = "file.updated"
	EventTypeFileMoved             EventType = "file.moved"
	EventTypeFileRenamed           EventType = "file.renamed"
	EventTypeFileTrashed           EventType = "file.trashed"
	EventTypeFileContentDownloaded EventType = "file.content.downloaded"
	EventTypeFileContentUploaded   EventType = "file.content.uploaded"
	EventTypeFileRestored          EventType = "file.restored"
	EventTypeFolderCreated         EventType = "folder.created"
	EventTypeFolderDeleted         EventType = "folder.deleted"
	EventTypeFolderMoved           EventType = "folder.moved"
	EventTypeFolderRenamed         EventType = "folder.renamed"
	EventTypeFolderTrashed         EventType = "folder.trashed"
	EventTypeFolderRestored        EventType = "folder.restored"
)

// FileTreeChangeEventTypes are the events that can require cache invalidation
var FileTreeChangeEventTypes = map[EventType]struct{}{
	EventTypeFileCreated:         {},
	EventTypeFileCopied:          {},
	EventTypeFileUpdated:         {},
	EventTypeFileMoved:           {},
	EventTypeFileRenamed:         {},
	EventTypeFileTrashed:         {},
	EventTypeFileContentUploaded: {},
	EventTypeFileRestored:        {},
	EventTypeFolderCreated:       {},
	EventTypeFolderDeleted:       {},
	EventTypeFolderMoved:         {},
	EventTypeFolderRenamed:       {},
	EventTypeFolderTrashed:       {},
	EventTypeFolderRestored:      {},
}

// EventData represents the common fields for event data
type EventData struct {
	EntityID     string `json:"entityId"`
	Path         string `json:"path,omitempty"`
	PreviousPath string `json:"previousPath,omitempty"`
}

// Event represents a single event from the events API
type Event struct {
	ID        string    `json:"id"`
	VaultID   string    `json:"vaultId"`
	Type      EventType `json:"type"`
	Data      EventData `json:"data"`
	AuthorID  *string   `json:"authorId"`
	CreatedAt Time      `json:"createdAt"`
}

// Events represents the response from the events API
type Events struct {
	HasMore     bool    `json:"hasMore"`
	Events      []Event `json:"events"`
	NewPosition Time    `json:"newPosition"`
}
