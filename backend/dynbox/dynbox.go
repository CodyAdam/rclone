package dynbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/rclone/rclone/backend/dynbox/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/random"
	"github.com/rclone/rclone/lib/rest"
)

const (
	minSleep               = 10 * time.Millisecond
	maxSleep               = 2 * time.Second
	decayConstant          = 2                           // bigger for slower decay, exponential
	uploadCutoff           = fs.SizeSuffix(50 * fs.Mebi) // bytes treshold for multipart upload
	accessCookieName       = "authjs.session-token"
	accessCookieNameSecure = "__Secure-authjs.session-token"
	rootID                 = "root"
)

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "dynbox",
		Description: "Dynbox",
		NewFs:       NewFs,
		MetadataInfo: &fs.MetadataInfo{
			Help: `For now, file metadata are not supported.`,
		},
		Config: nil,
		Options: []fs.Option{{
			Name:      "vault_id",
			Help:      "The vault ID to access",
			Required:  true,
			Sensitive: true,
		}, {
			Name:      "access_token",
			Help:      "Access token for authentication",
			Required:  true,
			Sensitive: true,
		}, {
			Name:     "endpoint",
			Help:     "API endpoint",
			Default:  "https://dynbox.co/api/fs",
			Advanced: true,
		},
			{
				Name:     config.ConfigEncoding,
				Help:     config.ConfigEncodingHelp,
				Advanced: true,
				Default: (encoder.Display |
					encoder.EncodeSlash |
					encoder.EncodeLtGt |
					encoder.EncodeDoubleQuote |
					encoder.EncodeAsterisk |
					encoder.EncodePipe |
					encoder.EncodeBackSlash |
					encoder.EncodeColon |
					encoder.EncodeQuestion |
					encoder.EncodeLeftSpace |
					encoder.EncodeRightSpace |
					encoder.EncodeRightPeriod |
					encoder.EncodeInvalidUtf8),
			}},
	})
}

// Options defines the configuration for this backend
type Options struct {
	VaultID     string               `config:"vault_id"`
	AccessToken string               `config:"access_token"`
	Endpoint    string               `config:"endpoint"`
	Enc         encoder.MultiEncoder `config:"encoding"`
}

// Fs represents a remote box
type Fs struct {
	name     string             // name of this remote
	root     string             // the path we are working on
	opt      Options            // parsed options
	features *fs.Features       // optional features
	srv      *rest.Client       // the connection to the server
	dirCache *dircache.DirCache // Map of directory path to directory id
	pacer    *fs.Pacer          // pacer for API calls
}

// Object describes a box object
//
// Will definitely have info but maybe not meta
type Object struct {
	hasMetaData bool
	fs          *Fs       // what this object is part of
	remote      string    // The remote path
	size        int64     // size of the object
	id          string    // ID of the object
	modTime     time.Time // modification time of the object
	hash        string    // SHA-256 of the object content
	contentType string    // MIME type of the object
	isDeleted   bool      // true if the object is deleted
}

// ------------------------------------------------------------

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// String converts this Fs to a string
func (f *Fs) String() string {
	return fmt.Sprintf("Dynbox root '%s'", f.root)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// Precision of the ModTimes in this Fs
func (f *Fs) Precision() time.Duration {
	return time.Second
}

// Hashes returns the supported hash types of the filesystem
func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.SHA256)
}

// retryErrorCodes is a slice of error codes that we will retry
var retryErrorCodes = []int{
	429, // Too Many Requests.
	500, // Internal Server Error
	502, // Bad Gateway
	503, // Service Unavailable
	504, // Gateway Timeout
	509, // Bandwidth Limit Exceeded
}

// shouldRetry returns a boolean as to whether this resp and err
// deserve to be retried.  It returns the err as a convenience
func shouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}

	authRetry := false
	if resp != nil && resp.StatusCode == 401 {
		authRetry = true
		fs.Debugf(nil, "Should retry: %v", err)
	}

	return authRetry || fserrors.ShouldRetry(err) || fserrors.ShouldRetryHTTP(resp, retryErrorCodes), err
}

// errorHandler parses a non 2xx error response into an error
func errorHandler(resp *http.Response) error {
	// Decode error response
	errResponse := new(api.Error)
	err := rest.DecodeJSON(resp, &errResponse)
	if err != nil {
		fs.Debugf(nil, "Couldn't decode error response: %v", err)
	}
	if errResponse.Code == "" {
		errResponse.Code = resp.Status
	}
	if errResponse.Status == 0 {
		errResponse.Status = resp.StatusCode
	}
	return errResponse
}

// encodePathSegment encodes a filename using URL encoding
// This replaces special characters with percent-encoded values
func (f *Fs) encodePathSegment(name string) string {
	return url.PathEscape(name)
}

// decodePathSegment decodes a URL-encoded filename back to its original form
// This converts percent-encoded values back to original characters
func (f *Fs) decodePathSegment(encodedName string) (string, error) {
	// Use the standard url package's PathUnescape
	decoded, err := url.PathUnescape(encodedName)
	if err != nil {
		return "", fmt.Errorf("failed to decode filename %q: %w", encodedName, err)
	}
	return decoded, nil
}

// decodePath decodes each segment of a path that is URL encoded
// It splits the path by '/', decodes each segment, and rejoins with '/'
func (f *Fs) decodePath(encodedPath string) (string, error) {
	// Split path into segments
	segments := strings.Split(encodedPath, "/")

	// Decode each segment
	for i, segment := range segments {
		if segment == "" {
			continue
		}
		decoded, err := f.decodePathSegment(segment)
		if err != nil {
			return "", fmt.Errorf("failed to decode path segment %q: %w", segment, err)
		}
		segments[i] = decoded
	}

	// Rejoin segments with /
	return strings.Join(segments, "/"), nil
}

// encodePath encodes each segment of a path using URL encoding
// It splits the path by '/', encodes each segment, and rejoins with '/'
func (f *Fs) encodePath(path string) string {
	// Split path into segments
	segments := strings.Split(path, "/")

	// Encode each segment
	for i, segment := range segments {
		if segment == "" {
			continue
		}
		segments[i] = f.encodePathSegment(segment)
	}

	// Rejoin segments with /
	return strings.Join(segments, "/")
}

// NewFs constructs an Fs from the path, container:path
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	// Parse config into Options struct
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}

	root = strings.Trim(root, "/")

	f := &Fs{
		name:  name,
		root:  root,
		opt:   *opt,
		srv:   rest.NewClient(fshttp.NewClient(ctx)).SetRoot(opt.Endpoint).SetErrorHandler(errorHandler),
		pacer: fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(minSleep), pacer.MaxSleep(maxSleep), pacer.DecayConstant(decayConstant))),
	}
	f.features = (&fs.Features{
		ReadMimeType:            true,
		WriteMimeType:           true,
		UserMetadata:            true,
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)

	// If using an accessToken, set the Authorization header
	if f.opt.AccessToken != "" {
		cookieName := accessCookieName
		if strings.HasPrefix(f.opt.Endpoint, "https") {
			cookieName = accessCookieNameSecure
		}
		f.srv.SetCookie(&http.Cookie{
			Name:  cookieName,
			Value: f.opt.AccessToken,
			Path:  "/",
		})
	}

	// Get rootFolderID
	f.dirCache = dircache.New(root, rootID, f)

	// Find the current root
	err = f.dirCache.FindRoot(ctx, false)
	fs.Debugf(f, "Finding root: %v", err)
	if err != nil {
		// Assume it is a file
		newRoot, remote := dircache.SplitPath(root)
		tempF := *f
		tempF.dirCache = dircache.New(newRoot, rootID, &tempF)
		tempF.root = newRoot
		// Make new Fs which is the parent
		err = tempF.dirCache.FindRoot(ctx, false)
		if err != nil {
			// No root so return old f
			return f, nil
		}
		_, err := tempF.newObjectWithInfo(ctx, remote, nil)
		if err != nil {
			if err == fs.ErrorObjectNotFound {
				// File doesn't exist so return old f
				return f, nil
			}
			return nil, err
		}
		f.features.Fill(ctx, &tempF)
		// XXX: update the old f here instead of returning tempF, since
		// `features` were already filled with functions having *f as a receiver.
		// See https://github.com/rclone/rclone/issues/2182
		f.dirCache = tempF.dirCache
		f.root = tempF.root
		// return an error with an fs which points to the parent
		return f, fs.ErrorIsFile
	}
	return f, nil
}

// rootSlash returns root with a slash on if it is empty, otherwise empty string
func (f *Fs) rootSlash() string {
	if f.root == "" {
		return f.root
	}
	return f.root + "/"
}

// About gets quota information
func (f *Fs) About(ctx context.Context) (usage *fs.Usage, err error) {
	opts := rest.Opts{
		Method: "GET",
		Path:   "/fs/usage",
		Parameters: url.Values{
			"vaultId": []string{f.opt.VaultID},
		},
	}
	var data api.Usage
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &data)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to read user info: %w", err)
	}
	usage = &fs.Usage{
		Used:    fs.NewUsageValue(data.FS.Used),        // bytes in use
		Total:   fs.NewUsageValue(data.FS.Total),       // bytes total
		Free:    fs.NewUsageValue(data.FS.Free),        // bytes free
		Trashed: fs.NewUsageValue(data.FS.Trashed),     // bytes trashed
		Other:   fs.NewUsageValue(data.FS.Other),       // bytes other
		Objects: fs.NewUsageValue(data.FS.ObjectCount), // objects
	}
	return usage, nil
}

// CreateDir makes a directory with pathID as parent and name leaf
func (f *Fs) CreateDir(ctx context.Context, pathID, leaf string) (newID string, err error) {
	fs.Debugf(f, "CreateDir(%q, %q)\n", pathID, leaf)
	var resp *http.Response
	var info *api.Item
	opts := rest.Opts{
		Method: "POST",
		Path:   "/fs/folders",
	}
	mkdir := api.CreateFolder{
		VaultID:  f.opt.VaultID,
		Name:     f.opt.Enc.FromStandardName(leaf),
		ParentID: &pathID,
	}
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, &mkdir, &info)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		fmt.Printf("...Error %v\n", err)
		return "", err
	}
	fmt.Printf("...Id %q\n", info.ID)
	return info.ID, nil
}

// NewObject finds the Object at remote. If it can't be found
// it returns the error fs.ErrorObjectNotFound.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	return f.newObjectWithInfo(ctx, remote, nil)
}

// Return an Object from a path
//
// If it can't be found it returns the error fs.ErrorObjectNotFound.
func (f *Fs) newObjectWithInfo(ctx context.Context, remote string, info *api.FileItem) (fs.Object, error) {
	o := &Object{
		fs:     f,
		remote: remote,
	}
	var err error
	if info != nil {
		// Set info
		err = o.setMetaData(info)
	} else {
		err = o.readMetaData(ctx) // reads info and meta, returning an error
	}
	if err != nil {
		return nil, err
	}
	return o, nil
}

// readMetaDataForPath reads the metadata from the path
func (f *Fs) readMetaDataForPath(ctx context.Context, path string) (info *api.FileItem, err error) {
	// defer log.Trace(f, "path=%q", path)("info=%+v, err=%v", &info, &err)
	leaf, directoryID, err := f.dirCache.FindPath(ctx, path, false)
	if err != nil {
		if err == fs.ErrorDirNotFound {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}

	// Use preupload to find the ID
	fileID, err := f.preUploadCheck(ctx, leaf, directoryID, -1)
	if err != nil {
		return nil, err
	}
	if fileID == nil {
		return nil, fs.ErrorObjectNotFound
	}

	// Now we have the ID we can look up the object proper
	opts := rest.Opts{
		Method: "GET",
		Path:   "/fs/files/" + *fileID,
	}
	var item api.FileItem
	err = f.pacer.Call(func() (bool, error) {
		resp, err := f.srv.CallJSON(ctx, &opts, nil, &item)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// FindLeaf finds a directory of name leaf in the folder with ID pathID
func (f *Fs) FindLeaf(ctx context.Context, pathID, leaf string) (pathIDOut string, found bool, err error) {
	// Find the leaf in pathID
	found, err = f.listAll(ctx, pathID, true, false, true, func(file *api.FileItem, folder *api.FolderItem) bool {
		if folder != nil && strings.EqualFold(folder.Name, leaf) {
			pathIDOut = folder.ID
			return true
		}
		return false
	})
	return pathIDOut, found, err
}

// list the objects into the function supplied
//
// If directories is set it only sends directories
// User function to process a File item from listAll
//
// Should return true to finish processing
type listAllFn func(*api.FileItem, *api.FolderItem) bool // (file (nil if not file), folder (nil if not folder))

// Lists the directory required calling the user function on each item found
//
// If the user fn ever returns true then it early exits with found = true
func (f *Fs) listAll(ctx context.Context, dirID string, directoriesOnly bool, filesOnly bool, activeOnly bool, fn listAllFn) (found bool, err error) {
	opts := rest.Opts{
		Method: "GET",
		Path:   "/fs/folders/" + dirID + "/items",
		Parameters: url.Values{
			"vaultId": []string{f.opt.VaultID},
		},
	}

	var result api.FolderItems
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &result)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return found, fmt.Errorf("couldn't list files: %w", err)
	}

	// Process folders
	if !filesOnly {
		for i := range result.Folders {
			item := &result.Folders[i]
			if activeOnly && item.IsDeleted() {
				continue
			}
			item.Name = f.opt.Enc.ToStandardName(item.Name)
			if fn(nil, item) {
				found = true
				return found, nil
			}
		}
	}

	// Process files
	if !directoriesOnly {
		for i := range result.Files {
			item := &result.Files[i]
			if activeOnly && item.IsDeleted() {
				continue
			}
			item.Name = f.opt.Enc.ToStandardName(item.Name)
			if fn(item, nil) {
				found = true
				return found, nil
			}
		}
	}

	return found, nil
}

// List the objects and directories in dir into entries.  The
// entries can be returned in any order but should be for a
// complete directory.
//
// dir should be "" to list the root, and should not have
// trailing slashes.
//
// This should return ErrDirNotFound if the directory isn't
// found.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	directoryID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return nil, err
	}
	var iErr error
	_, err = f.listAll(ctx, directoryID, false, false, true, func(file *api.FileItem, folder *api.FolderItem) bool {
		var remote string
		switch {
		case folder != nil:
			remote = path.Join(dir, folder.Name)
			// cache the directory ID for later lookups
			f.dirCache.Put(remote, folder.ID)
			d := fs.NewDir(remote, folder.UpdateTime()).SetID(folder.ID)
			entries = append(entries, d)
		case file != nil:
			remote = path.Join(dir, file.Name)
			// Create a file object (or update it)
			o, err := f.newObjectWithInfo(ctx, remote, file)
			if err != nil {
				iErr = err
				return true
			}
			entries = append(entries, o)
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	if iErr != nil {
		return nil, iErr
	}
	return entries, nil
}

// preUploadCheck checks to see if a file can be uploaded
//
// It returns nil, nil if the file is good to go
// It returns FileItem, nil if the file must be updated
func (f *Fs) preUploadCheck(ctx context.Context, leaf, directoryID string, size int64) (item *string, err error) {
	check := api.PreUploadCheck{
		Name:     f.opt.Enc.FromStandardName(leaf),
		FolderID: &directoryID,
		Size:     size,
		VaultID:  f.opt.VaultID,
	}
	opts := rest.Opts{
		Method: "OPTIONS",
		Path:   "/fs/files/upload",
	}
	var result *api.PreUploadCheckResponse
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, &check, &result)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return nil, err
	}
	return result.ExistingFileID, nil
}

// Put the object
//
// Copy the reader in to the new object which is returned.
//
// The new object may have been created if an error is returned
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	// If directory doesn't exist, file doesn't exist so can upload
	remote := src.Remote()
	leaf, directoryID, err := f.dirCache.FindPath(ctx, remote, false)
	if err != nil {
		if err == fs.ErrorDirNotFound {
			return f.PutUnchecked(ctx, in, src, options...)
		}
		return nil, err
	}

	// Preflight check the upload, which returns the ID if the
	// object already exists
	fileID, err := f.preUploadCheck(ctx, leaf, directoryID, src.Size())
	if err != nil {
		return nil, err
	}
	if fileID == nil {
		return f.PutUnchecked(ctx, in, src, options...)
	}

	// If object exists then create a skeleton one with just id, size, hash, type
	o := &Object{
		fs:     f,
		remote: remote,
		id:     *fileID,
	}
	return o, o.Update(ctx, in, src, options...)
}

// PutUnchecked the object into the container
//
// This will produce an error if the object already exists.
//
// Copy the reader in to the new object which is returned.
//
// The new object may have been created if an error is returned
func (f *Fs) PutUnchecked(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	remote := src.Remote()
	size := src.Size()
	modTime := src.ModTime(ctx)

	o, _, _, err := f.createObject(ctx, remote, modTime, size)
	if err != nil {
		return nil, err
	}
	return o, o.Update(ctx, in, src, options...)
}

// Creates from the parameters passed in a half finished Object which
// must have setMetaData called on it
//
// Returns the object, leaf, directoryID and error.
//
// Used to create new objects
func (f *Fs) createObject(ctx context.Context, remote string, modTime time.Time, size int64) (o *Object, leaf string, directoryID string, err error) {
	// Create the directory for the object if it doesn't exist
	leaf, directoryID, err = f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return
	}
	// Temporary Object under construction
	o = &Object{
		fs:      f,
		modTime: modTime,
		size:    size,
		remote:  remote,
	}
	return o, leaf, directoryID, nil
}

// Mkdir creates the container if it doesn't exist
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	_, err := f.dirCache.FindDir(ctx, dir, true)
	return err
}

// deleteObject removes an object by ID
func (f *Fs) deleteObject(ctx context.Context, id string) error {
	opts := rest.Opts{
		Method:     "DELETE",
		Path:       "/fs/files/" + id,
		NoResponse: true,
	}
	input := api.DeleteFile{
		Permanent: false,
	}
	return f.pacer.Call(func() (bool, error) {
		resp, err := f.srv.CallJSON(ctx, &opts, &input, nil)
		return shouldRetry(ctx, resp, err)
	})
}

// purgeCheck removes the root directory, if check is set then it
// refuses to do so if it has anything in
func (f *Fs) purgeCheck(ctx context.Context, dir string, check bool) error {
	root := path.Join(f.root, dir)
	if root == "" {
		return errors.New("can't purge root directory")
	}
	dc := f.dirCache
	rootID, err := dc.FindDir(ctx, dir, false)
	if err != nil {
		return err
	}

	opts := rest.Opts{
		Method:     "DELETE",
		Path:       "/fs/folders/" + rootID,
		NoResponse: true,
	}
	input := api.PurgeCheck{
		FolderID:  &rootID,
		VaultID:   f.opt.VaultID,
		Recursive: !check,
		Permanent: false,
	}
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, &input, nil)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return fmt.Errorf("purge failed: %w", err)
	}
	f.dirCache.FlushDir(dir)
	if err != nil {
		return err
	}
	return nil
}

// Rmdir deletes the root folder
//
// Returns an error if it isn't empty
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	return f.purgeCheck(ctx, dir, true)
}

// Copy src to this remote using server-side copy operations.
//
// This is stored with the remote path given.
//
// It returns the destination Object and a possible error.
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantCopy
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't copy - not same remote type")
		return nil, fs.ErrorCantCopy
	}
	err := srcObj.readMetaData(ctx)
	if err != nil {
		return nil, err
	}

	srcPath := srcObj.fs.rootSlash() + srcObj.remote
	dstPath := f.rootSlash() + remote
	if strings.EqualFold(srcPath, dstPath) {
		return nil, fmt.Errorf("can't copy %q -> %q as are same name when lowercase", srcPath, dstPath)
	}

	// Create temporary object
	dstObj, leaf, directoryID, err := f.createObject(ctx, remote, srcObj.modTime, srcObj.size)
	if err != nil {
		return nil, err
	}

	// check if dest already exists
	fileID, err := f.preUploadCheck(ctx, leaf, directoryID, src.Size())
	if err != nil {
		return nil, err
	}
	if fileID != nil { // dest already exists, need to copy to temp name and then move
		tempSuffix := "-rclone-copy-" + random.String(8)
		fs.Debugf(remote, "dst already exists, copying to temp name %v", remote+tempSuffix)
		tempObj, err := f.Copy(ctx, src, remote+tempSuffix)
		if err != nil {
			return nil, err
		}
		fs.Debugf(remote+tempSuffix, "moving to real name %v", remote)
		err = f.deleteObject(ctx, *fileID)
		if err != nil {
			return nil, err
		}
		return f.Move(ctx, tempObj, remote)
	}

	// Copy the object
	opts := rest.Opts{
		Method: "POST",
		Path:   "/fs/files/" + srcObj.id + "/copy",
	}
	copyFile := api.CopyFile{
		NewParentID: &directoryID,
		NewName:     f.opt.Enc.FromStandardName(leaf),
	}
	var resp *http.Response
	var info *api.FileItem
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, &copyFile, &info)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return nil, err
	}
	err = dstObj.setMetaData(info)
	if err != nil {
		return nil, err
	}
	return dstObj, nil
}

// Purge deletes all the files and the container
//
// Optional interface: Only implement this if you have a way of
// deleting all the files quicker than just running Remove() on the
// result of List()
func (f *Fs) Purge(ctx context.Context, dir string) error {
	return f.purgeCheck(ctx, dir, false)
}

// Move src to this remote using server-side move operations.
//
// This is stored with the remote path given.
//
// It returns the destination Object and a possible error.
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantMove
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't move - not same remote type")
		return nil, fs.ErrorCantMove
	}

	// Create temporary object
	dstObj, leaf, directoryID, err := f.createObject(ctx, remote, srcObj.modTime, srcObj.size)
	if err != nil {
		return nil, err
	}

	// Do the move
	info, err := f.move(ctx, true, srcObj.id, leaf, directoryID)
	if err != nil {
		return nil, err
	}

	err = dstObj.setMetaData(info)
	if err != nil {
		return nil, err
	}
	return dstObj, nil
}

// DirMove moves src, srcRemote to this remote at dstRemote
// using server-side move operations.
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantDirMove
//
// If destination exists then return fs.ErrorDirExists
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	srcFs, ok := src.(*Fs)
	if !ok {
		fs.Debugf(srcFs, "Can't move directory - not same remote type")
		return fs.ErrorCantDirMove
	}

	srcID, _, _, dstDirectoryID, dstLeaf, err := f.dirCache.DirMove(ctx, srcFs.dirCache, srcFs.root, srcRemote, f.root, dstRemote)
	if err != nil {
		return err
	}

	// Do the move
	_, err = f.move(ctx, false, srcID, dstLeaf, dstDirectoryID)
	if err != nil {
		return err
	}
	srcFs.dirCache.FlushDir(srcRemote)
	return nil
}

// Move a file or folder
//
// Endpoint is either "/files/" or "/folders/"
func (f *Fs) move(ctx context.Context, isFile bool, id, leaf, directoryID string) (info *api.FileItem, err error) {
	// Move the object
	var endpoint string
	if isFile {
		endpoint = "/fs/files/" + id + "/move"
	} else {
		endpoint = "/fs/folders/" + id + "/move"
	}
	opts := rest.Opts{
		Method: "POST",
		Path:   endpoint,
	}
	move := api.Move{
		VaultID:     f.opt.VaultID,
		NewParentID: &directoryID,
		NewName:     f.opt.Enc.FromStandardName(leaf),
	}
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, &move, &info)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		if apiErr, ok := err.(*api.Error); ok && apiErr.Code == "CONFLICT" {
			if isFile {
				return nil, fs.ErrorCantMove
			}
			return nil, fs.ErrorCantDirMove
		}
		return nil, err
	}
	if isFile {
		return info, nil
	}
	return nil, nil
}

// FIXME Public Links are not supported yet
// // PublicLink adds a "readable by anyone with link" permission on the given file or folder.
// func (f *Fs) PublicLink(ctx context.Context, remote string, expire fs.Duration, unlink bool) (string, error) {
// 	id, err := f.dirCache.FindDir(ctx, remote, false)
// 	var opts rest.Opts
// 	if err == nil { // is a directory
// 		fs.Debugf(f, "attempting to share directory '%s'", remote)

// 		opts = rest.Opts{
// 			Method: "PUT",
// 			Path:   "/folders/" + id,
// 		}
// 	} else { // is a file
// 		fs.Debugf(f, "attempting to share single file '%s'", remote)
// 		o, err := f.NewObject(ctx, remote)
// 		if err != nil {
// 			return "", err
// 		}

// 		if o.(*Object).publicLink != "" {
// 			return o.(*Object).publicLink, nil
// 		}

// 		opts = rest.Opts{
// 			Method: "PUT",
// 			Path:   "/files/" + o.(*Object).id,
// 		}
// 	}

// 	shareLink := api.CreateSharedLink{}
// 	var info api.Item
// 	var resp *http.Response
// 	err = f.pacer.Call(func() (bool, error) {
// 		resp, err = f.srv.CallJSON(ctx, &opts, &shareLink, &info)
// 		return shouldRetry(ctx, resp, err)
// 	})
// 	return info.SharedLink.URL, err
// }

// CleanUp empties the trash
func (f *Fs) CleanUp(ctx context.Context) (err error) {
	opts := rest.Opts{
		Method:     "DELETE",
		Path:       "/fs/trash",
		NoResponse: true,
	}
	input := api.Vault{
		ID: f.opt.VaultID,
	}

	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, &input, nil)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return err
	}
	return nil
}

// ChangeNotify calls the passed function with a path that has had changes.
// If the implementation uses polling, it should adhere to the given interval.
//
// Automatically restarts itself in case of unexpected behavior of the remote.
//
// Close the returned channel to stop being notified.
func (f *Fs) ChangeNotify(ctx context.Context, notifyFunc func(string, fs.EntryType), pollIntervalChan <-chan time.Duration) {
	go func() {
		// Start tracking changes from now
		streamPosition := time.Now()

		// box can send duplicate Event IDs. Use this map to track and filter
		// the ones we've already processed.
		processedEventIDs := make(map[string]time.Time)

		var ticker *time.Ticker
		var tickerC <-chan time.Time
		for {
			select {
			case pollInterval, ok := <-pollIntervalChan:
				if !ok {
					if ticker != nil {
						ticker.Stop()
					}
					return
				}
				if ticker != nil {
					ticker.Stop()
					ticker, tickerC = nil, nil
				}
				if pollInterval != 0 {
					ticker = time.NewTicker(pollInterval)
					tickerC = ticker.C
				}
			case <-tickerC:
				// Garbage collect EventIDs older than 1 minute
				for eventID, timestamp := range processedEventIDs {
					if time.Since(timestamp) > time.Minute {
						delete(processedEventIDs, eventID)
					}
				}

				var err error
				streamPosition, err = f.changeNotifyRunner(ctx, notifyFunc, streamPosition, processedEventIDs)
				if err != nil {
					fs.Infof(f, "Change notify listener failure: %s", err)
				}
			}
		}
	}()
}

func (f *Fs) changeNotifyRunner(ctx context.Context, notifyFunc func(string, fs.EntryType), streamPosition time.Time, processedEventIDs map[string]time.Time) (nextStreamPosition time.Time, err error) {
	opts := rest.Opts{
		Method: "GET",
		Path:   "/events/" + f.opt.VaultID,
		Parameters: url.Values{
			"datePosition": []string{streamPosition.Format(time.RFC3339)},
		},
	}

	var result api.Events
	var resp *http.Response
	fs.Debugf(f, "Checking for changes on remote (date_position: %v)", streamPosition)
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &result)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return streamPosition, err
	}

	if len(result.Events) == 0 {
		return time.Time(result.NewPosition), nil
	}

	type pathToClear struct {
		path      string
		entryType fs.EntryType
	}
	var pathsToClear []pathToClear
	newEventIDs := 0

	for _, event := range result.Events {
		// Skip already processed events
		if _, ok := processedEventIDs[event.ID]; ok {
			continue
		}
		processedEventIDs[event.ID] = time.Now()
		newEventIDs++

		// Determine entry type based on event type
		var entryType fs.EntryType
		if strings.HasPrefix(string(event.Type), "file.") {
			entryType = fs.EntryObject
		} else if strings.HasPrefix(string(event.Type), "folder.") {
			entryType = fs.EntryDirectory
		} else {
			continue
		}

		// Get all paths that need to be checked for this event
		paths := f.getPathsFromEventData(event)
		for _, path := range paths {
			if path == "" {
				continue
			}

			pathsToClear = append(pathsToClear, pathToClear{
				path:      path,
				entryType: entryType,
			})

			// If this is a directory event, flush the directory cache
			if entryType == fs.EntryDirectory {
				f.dirCache.FlushDir(path)
			}
		}
	}

	// Notify about all changes
	notifiedPaths := make(map[string]bool)
	for _, p := range pathsToClear {
		if _, ok := notifiedPaths[p.path]; ok {
			continue
		}
		notifiedPaths[p.path] = true
		notifyFunc(p.path, p.entryType)
	}

	fs.Debugf(f, "Received %v events, resulting in %v paths and %v notifications",
		len(result.Events), len(pathsToClear), len(notifiedPaths))

	return time.Time(result.NewPosition), nil
}

// getPathsFromEventData extracts all relevant file/folder paths from the event data
func (f *Fs) getPathsFromEventData(event api.Event) []string {
	var paths []string

	// Helper function to process a path
	processPath := func(path string) {
		decodedPath, err := f.decodePath(path)
		if err != nil {
			fs.Debugf(f, "Failed to decode path %q: %v", path, err)
			return
		}
		// Remove leading slash if present
		relativePath := strings.TrimPrefix(decodedPath, "/")
		// Convert absolute path to relative path by removing remote root prefix
		relativePath = strings.TrimPrefix(relativePath, f.root)
		relativePath = strings.TrimPrefix(relativePath, "/")
		paths = append(paths, relativePath)
	}

	// Process current path if available
	if event.Data.Path != "" {
		processPath(event.Data.Path)
	}

	// Process previous path for operations like move/rename
	if event.Data.PreviousPath != "" {
		processPath(event.Data.PreviousPath)
	}

	return paths
}

// DirCacheFlush resets the directory cache - used in testing as an
// optional interface
func (f *Fs) DirCacheFlush() {
	f.dirCache.ResetRoot()
}

// ------------------------------------------------------------

// Fs returns the parent Fs
func (o *Object) Fs() fs.Info {
	return o.fs
}

// Return a string version
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// Remote returns the remote path
func (o *Object) Remote() string {
	return o.remote
}

// Hash returns the SHA-256 of an object returning a lowercase hex string
func (o *Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	if t != hash.SHA256 {
		return "", hash.ErrUnsupported
	}
	return o.hash, nil
}

// Size returns the size of an object in bytes
func (o *Object) Size() int64 {
	err := o.readMetaData(context.TODO())
	if err != nil {
		fs.Logf(o, "Failed to read metadata: %v", err)
		return 0
	}
	return o.size
}

// setMetaData sets the metadata from info
func (o *Object) setMetaData(info *api.FileItem) (err error) {
	o.hasMetaData = true
	o.id = info.ID
	o.size = info.Size
	o.hash = info.Hash
	o.contentType = info.ContentType
	o.modTime = info.ModTime()
	o.isDeleted = info.IsDeleted()
	return nil
}

// readMetaData gets the metadata if it hasn't already been fetched
//
// it also sets the info
func (o *Object) readMetaData(ctx context.Context) (err error) {
	if o.hasMetaData {
		return nil
	}
	info, err := o.fs.readMetaDataForPath(ctx, o.remote)
	if err != nil {
		if apiErr, ok := err.(*api.Error); ok {
			if apiErr.Code == "NOT_FOUND" {
				return fs.ErrorObjectNotFound
			}
		}
		return err
	}
	return o.setMetaData(info)
}

// ModTime returns the modification time of the object
//
// It attempts to read the objects mtime and if that isn't present the
// LastModified returned in the http headers
func (o *Object) ModTime(ctx context.Context) time.Time {
	err := o.readMetaData(ctx)
	if err != nil {
		fs.Logf(o, "Failed to read metadata: %v", err)
		return time.Now()
	}
	return o.modTime
}

// setModTime sets the modification time of the local fs object
func (o *Object) setModTime(ctx context.Context, modTime time.Time) (*api.FileItem, error) {
	opts := rest.Opts{
		Method: "PUT",
		Path:   "/fs/files/" + o.id,
	}
	update := api.UpdateFileMetadata{
		ModTime: api.Time(modTime),
	}
	var info *api.FileItem
	err := o.fs.pacer.Call(func() (bool, error) {
		resp, err := o.fs.srv.CallJSON(ctx, &opts, &update, &info)
		return shouldRetry(ctx, resp, err)
	})
	return info, err
}

// SetModTime sets the modification time of the local fs object
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	if !o.hasMetaData {
		err := o.readMetaData(ctx)
		if err != nil {
			return err
		}
	}
	info, err := o.setModTime(ctx, modTime)
	if err != nil {
		return err
	}
	return o.setMetaData(info)
}

// Storable returns a boolean showing whether this object storable
func (o *Object) Storable() bool {
	return true
}

// Open an object for read
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (in io.ReadCloser, err error) {
	if o.id == "" {
		return nil, errors.New("can't download - no id")
	}
	fs.FixRangeOption(options, o.size)
	var resp *http.Response
	opts := rest.Opts{
		Method:  "GET",
		Path:    "/fs/files/" + o.id + "/content",
		Options: options,
	}
	err = o.fs.pacer.Call(func() (bool, error) {
		resp, err = o.fs.srv.Call(ctx, &opts)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return nil, err
	}
	return resp.Body, err
}

// generateContentHash generates the SHA-256 hash of the object's content before uploading
func (o *Object) generateContentHash(in io.Reader) (string, error) {
	// Create a new SHA-256 hasher
	hasher, err := hash.NewMultiHasherTypes(hash.NewHashSet(hash.SHA256))
	if err != nil {
		return "", err
	}

	// Copy the input to the hasher
	_, err = io.Copy(hasher, in)
	if err != nil {
		return "", err
	}

	// Get the hash as a hex string
	sums := hasher.Sums()
	return sums[hash.SHA256], nil
}

// Update the object with the contents of the io.Reader, modTime and size
//
// If existing is set then it updates the object rather than creating a new one.
//
// The new object may have been created if an error is returned.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (err error) {
	size := src.Size()
	modTime := src.ModTime(ctx)
	remote := o.Remote()

	// Create the directory for the object if it doesn't exist
	leaf, directoryID, err := o.fs.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return err
	}

	// Read all content first since we need to use it twice
	// (once for hash verification and once for upload)
	allBytes, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("failed to read upload content: %w", err)
	}

	// First pass: verify hash
	hashReader := bytes.NewReader(allBytes)
	hash, err := o.generateContentHash(hashReader)
	if err != nil {
		return err
	}

	contentReader := bytes.NewReader(allBytes)
	// Upload with simple or multipart
	if o.size <= int64(uploadCutoff) {
		err = o.upload(ctx, contentReader, leaf, directoryID, size, modTime, hash, options...)
	} else {
		err = o.uploadMultipart(ctx, contentReader, leaf, directoryID, size, modTime, hash, options...)
	}
	return err
}

// upload does a single non-multipart upload
//
// This is recommended for less than 50 MiB of content
func (o *Object) upload(ctx context.Context, in io.Reader, leaf, directoryID string, size int64, modTime time.Time, hash string, options ...fs.OpenOption) (err error) {
	// Create new file
	uploadReq := api.RequestUploadCreate{
		Name:     o.fs.opt.Enc.FromStandardName(leaf),
		Size:     size,
		VaultID:  o.fs.opt.VaultID,
		Hash:     hash,
		ModTime:  api.Time(modTime),
		ParentID: &directoryID,
	}

	fs.Debugf(o, "modTime: %v", modTime)

	var resp *http.Response
	var uploadResp api.UploadRequestResponse
	opts := rest.Opts{
		Method: "POST",
		Path:   "/fs/files/upload/single",
	}

	err = o.fs.pacer.CallNoRetry(func() (bool, error) {
		resp, err = o.fs.srv.CallJSON(ctx, &opts, uploadReq, &uploadResp)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return err
	}
	if uploadResp.UploadUrl == nil {
		// No upload needed - file was cached
		return o.readMetaData(ctx)
	}

	// Upload the file to the provided signed URL
	opts = rest.Opts{
		Method:        "PUT",
		RootURL:       *uploadResp.UploadUrl,
		Body:          in,
		ContentLength: &size, // Add explicit content length
		Options:       options,
	}

	err = o.fs.pacer.CallNoRetry(func() (bool, error) {
		resp, err = o.fs.srv.Call(ctx, &opts)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return err
	}

	return o.finishUpload(ctx, uploadResp.Key)
}

// finishUpload finalises the upload and update the metadata of the object
func (o *Object) finishUpload(ctx context.Context, key string) (err error) {
	opts := rest.Opts{
		Method: "POST",
		Path:   "/fs/files/upload/finished",
	}
	input := api.RequestUploadFinished{
		Key: key,
	}
	var result *api.FileItem
	var resp *http.Response
	err = o.fs.pacer.CallNoRetry(func() (bool, error) {
		resp, err = o.fs.srv.CallJSON(ctx, &opts, &input, &result)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return err
	}
	return o.setMetaData(result)
}

// Remove an object
func (o *Object) Remove(ctx context.Context) error {
	return o.fs.deleteObject(ctx, o.id)
}

// ID returns the ID of the Object if known, or "" if not
func (o *Object) ID() string {
	return o.id
}

// get object mime type
func (o *Object) MimeType(ctx context.Context) string {
	return o.contentType
}

// Check the interfaces are satisfied
var (
	_ fs.Fs              = (*Fs)(nil)
	_ fs.Purger          = (*Fs)(nil)
	_ fs.Copier          = (*Fs)(nil)
	_ fs.Abouter         = (*Fs)(nil)
	_ fs.Mover           = (*Fs)(nil)
	_ fs.DirMover        = (*Fs)(nil)
	_ fs.DirCacheFlusher = (*Fs)(nil)
	// _ fs.PublicLinker    = (*Fs)(nil)
	_ fs.CleanUpper = (*Fs)(nil)
	_ fs.Object     = (*Object)(nil)
	_ fs.MimeTyper  = (*Object)(nil)
	_ fs.IDer       = (*Object)(nil)
)
