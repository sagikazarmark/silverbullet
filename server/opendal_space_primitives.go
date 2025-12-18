package server

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	ignore "github.com/Diogenesoftoronto/go-gitignore"
	opendal "github.com/apache/opendal/bindings/go"
)

// OpenDALSpacePrimitives implements SpacePrimitives using OpenDAL
// This allows SilverBullet to use any storage backend supported by OpenDAL:
// S3, Azure Blob, Google Cloud Storage, local filesystem, and more
type OpenDALSpacePrimitives struct {
	operator  *opendal.Operator
	gitIgnore *ignore.GitIgnore
	rootPath  string
}

var _ SpacePrimitives = &OpenDALSpacePrimitives{}

// NewOpenDALSpacePrimitives creates a new OpenDAL-based SpacePrimitives
// operator: The OpenDAL operator configured for your storage backend
// rootPath: Optional prefix path within the storage backend
// gitIgnore: Gitignore pattern string for filtering files
func NewOpenDALSpacePrimitives(operator *opendal.Operator, rootPath string, gitIgnore string) (*OpenDALSpacePrimitives, error) {
	if operator == nil {
		return nil, fmt.Errorf("operator cannot be nil")
	}

	// Normalize root path
	rootPath = strings.Trim(rootPath, "/")
	if rootPath != "" {
		rootPath = rootPath + "/"
	}

	ignore := ignore.CompileIgnoreLines(strings.Split(gitIgnore, "\n")...)

	return &OpenDALSpacePrimitives{
		operator:  operator,
		gitIgnore: ignore,
		rootPath:  rootPath,
	}, nil
}

// Close releases resources associated with the OpenDAL operator
func (o *OpenDALSpacePrimitives) Close() error {
	if o.operator != nil {
		o.operator.Close()
	}
	return nil
}

// pathToStoragePath converts a relative path to the full storage path
func (o *OpenDALSpacePrimitives) pathToStoragePath(path string) string {
	// Clean the path and normalize to forward slashes
	cleanPath := filepath.Clean(path)
	cleanPath = strings.ReplaceAll(cleanPath, string(filepath.Separator), "/")
	cleanPath = strings.TrimPrefix(cleanPath, "/")

	return o.rootPath + cleanPath
}

// storagePathToPath converts a storage path back to a relative path
func (o *OpenDALSpacePrimitives) storagePathToPath(storagePath string) string {
	if o.rootPath == "" {
		return storagePath
	}
	return strings.TrimPrefix(storagePath, o.rootPath)
}

// metadataToFileMeta converts OpenDAL metadata to FileMeta
func (o *OpenDALSpacePrimitives) metadataToFileMeta(path string, meta *opendal.Metadata) FileMeta {
	// Get modification time (LastModified returns time.Time, not *time.Time)
	modTime := meta.LastModified()

	return FileMeta{
		Name:         path,
		Size:         int64(meta.ContentLength()),
		ContentType:  LookupContentTypeFromPath(path),
		Created:      modTime.UnixMilli(), // OpenDAL doesn't always expose creation time
		LastModified: modTime.UnixMilli(),
		Perm:         "rw",
	}
}

// FetchFileList implements SpacePrimitives.FetchFileList
// Returns a list of files in the space, with the following filtering rules:
// - Hidden files and directories (starting with '.') are excluded at any depth
// - Files without extensions are excluded
// - Files matching gitignore patterns are excluded
func (o *OpenDALSpacePrimitives) FetchFileList() ([]FileMeta, error) {
	var l sync.Mutex
	var allFiles []FileMeta

	// List all entries recursively
	lister, err := o.operator.List(o.rootPath)
	if err != nil {
		return nil, fmt.Errorf("failed to list files: %w", err)
	}
	defer lister.Close()

	for lister.Next() {
		entry := lister.Entry()
		storagePath := entry.Path()
		relativePath := o.storagePathToPath(storagePath)

		// Get metadata to check if it's a file or directory
		meta, err := o.operator.Stat(storagePath)
		if err != nil {
			// Skip entries we can't stat
			continue
		}

		// Skip directories
		if meta.IsDir() {
			continue
		}

		// Skip hidden files and files in hidden directories
		pathParts := strings.Split(relativePath, "/")
		hasHidden := false
		for _, part := range pathParts {
			if strings.HasPrefix(part, ".") {
				hasHidden = true
				break
			}
		}
		if hasHidden {
			continue
		}

		// Exclude ignored files
		if o.gitIgnore.MatchesPath(relativePath) {
			continue
		}

		// Skip files without extensions
		if filepath.Ext(relativePath) == "" {
			continue
		}

		fileMeta := o.metadataToFileMeta(relativePath, meta)
		l.Lock()
		allFiles = append(allFiles, fileMeta)
		l.Unlock()
	}

	if err := lister.Error(); err != nil {
		return nil, fmt.Errorf("error during file listing: %w", err)
	}

	return allFiles, nil
}

// GetFileMeta implements SpacePrimitives.GetFileMeta
func (o *OpenDALSpacePrimitives) GetFileMeta(path string) (FileMeta, error) {
	storagePath := o.pathToStoragePath(path)

	meta, err := o.operator.Stat(storagePath)
	if err != nil {
		// Check if it's a not found error (IsExist returns bool, error)
		exists, checkErr := o.operator.IsExist(storagePath)
		if checkErr != nil || !exists {
			return FileMeta{}, ErrNotFound
		}
		return FileMeta{}, fmt.Errorf("%w: %s", ErrCouldNotGetMeta, path)
	}

	// Ensure it's a file, not a directory
	if meta.IsDir() {
		return FileMeta{}, ErrNotFound
	}

	return o.metadataToFileMeta(path, meta), nil
}

// ReadFile implements SpacePrimitives.ReadFile
func (o *OpenDALSpacePrimitives) ReadFile(path string) ([]byte, FileMeta, error) {
	storagePath := o.pathToStoragePath(path)

	// Read file content
	data, err := o.operator.Read(storagePath)
	if err != nil {
		// Check if it's a not found error (IsExist returns bool, error)
		exists, checkErr := o.operator.IsExist(storagePath)
		if checkErr != nil || !exists {
			return nil, FileMeta{}, ErrNotFound
		}
		return nil, FileMeta{}, fmt.Errorf("failed to read file %s: %w", path, err)
	}

	// Get metadata
	meta, err := o.operator.Stat(storagePath)
	if err != nil {
		return nil, FileMeta{}, fmt.Errorf("failed to stat file %s: %w", path, err)
	}

	fileMeta := o.metadataToFileMeta(path, meta)
	return data, fileMeta, nil
}

// WriteFile implements SpacePrimitives.WriteFile
func (o *OpenDALSpacePrimitives) WriteFile(path string, data []byte, meta *FileMeta) (FileMeta, error) {
	storagePath := o.pathToStoragePath(path)

	// Write file content
	err := o.operator.Write(storagePath, data)
	if err != nil {
		return FileMeta{}, fmt.Errorf("%w: %s", ErrCouldNotWrite, path)
	}

	// Note: OpenDAL doesn't support setting custom modification times directly
	// The storage backend will set its own timestamps
	// If you need to preserve timestamps, you could store them as metadata
	// using storage backend-specific features (e.g., S3 user-defined metadata)

	// Return actual metadata
	return o.GetFileMeta(path)
}

// DeleteFile implements SpacePrimitives.DeleteFile
func (o *OpenDALSpacePrimitives) DeleteFile(path string) error {
	storagePath := o.pathToStoragePath(path)

	// Check if file exists first (IsExist returns bool, error)
	exists, err := o.operator.IsExist(storagePath)
	if err != nil {
		return fmt.Errorf("failed to check file existence %s: %w", path, err)
	}
	if !exists {
		return ErrNotFound
	}

	err = o.operator.Delete(storagePath)
	if err != nil {
		return fmt.Errorf("failed to delete file %s: %w", path, err)
	}

	// Note: Unlike DiskSpacePrimitives, we don't clean up empty "directories"
	// because most object stores (like S3) don't have real directories
	// They're just key prefixes

	return nil
}
