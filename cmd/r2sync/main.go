package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gofika/fikamime"
)

type R2Client struct {
	client *s3.Client
	bucket string
	scheme string
}

type FileInfo struct {
	Path         string
	Size         int64
	LastModified time.Time
	ETag         string
}

func NewR2Client(bucket, scheme string) *R2Client {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		log.Fatal(err)
	}

	return &R2Client{
		client: s3.NewFromConfig(cfg),
		bucket: bucket,
		scheme: scheme,
	}
}

func (r *R2Client) RemotePath(path string) string {
	return fmt.Sprintf("%s://%s/%s", r.scheme, r.bucket, path)
}

// List remote files
func (r *R2Client) ListObjects(prefix string) (map[string]FileInfo, error) {
	result := make(map[string]FileInfo)
	var continuationToken *string

	for {
		input := &s3.ListObjectsV2Input{
			Bucket: aws.String(r.bucket),
			Prefix: aws.String(prefix),
		}
		if continuationToken != nil {
			input.ContinuationToken = continuationToken
		}

		resp, err := r.client.ListObjectsV2(context.TODO(), input)
		if err != nil {
			return nil, err
		}

		for _, obj := range resp.Contents {
			result[*obj.Key] = FileInfo{
				Path:         *obj.Key,
				Size:         *obj.Size,
				LastModified: *obj.LastModified,
				ETag:         *obj.ETag,
			}
		}

		if !*resp.IsTruncated {
			break
		}
		continuationToken = resp.NextContinuationToken
	}

	return result, nil
}

// format speed display
func formatSpeed(bytesPerSecond float64) string {
	units := []string{"B/s", "KB/s", "MB/s", "GB/s", "TB/s"}
	unit := 0
	speed := bytesPerSecond

	for speed >= 1024 && unit < len(units)-1 {
		speed /= 1024
		unit++
	}

	return fmt.Sprintf("%.2f %s", speed, units[unit])
}

func formatSize(size int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	unit := 0
	bytes := float64(size)

	for bytes >= 1024 && unit < len(units)-1 {
		bytes /= 1024
		unit++
	}

	return fmt.Sprintf("%.2f %s", bytes, units[unit])
}

func (r *R2Client) UploadFile(localPath, remotePath string, dryRun bool) error {
	if dryRun {
		log.Printf("(dryrun) upload: %s -> %s\n", localPath, r.RemotePath(remotePath))
		return nil
	}

	file, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return err
	}

	startTime := time.Now()

	// Guess MIME type based on file extension
	ext := path.Ext(localPath)
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = fikamime.TypeByExtension(ext)
		if contentType == "" {
			contentType = "application/octet-stream" // Default type
		}
	}

	_, err = r.client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:        aws.String(r.bucket),
		Key:           aws.String(remotePath),
		Body:          file,
		ContentLength: aws.Int64(fileInfo.Size()),
		ContentType:   aws.String(contentType),
	})

	if err != nil {
		return err
	}

	elapsedTime := time.Since(startTime).Seconds()
	bytesPerSecond := float64(fileInfo.Size()) / elapsedTime
	speedStr := formatSpeed(bytesPerSecond)
	sizeStr := formatSize(fileInfo.Size())
	log.Printf("upload: %s -> %s, size: %s, average speed: %s\n", localPath, r.RemotePath(remotePath), sizeStr, speedStr)

	return nil
}

func (r *R2Client) DeleteObject(remotePath string, dryRun bool) error {
	if dryRun {
		log.Printf("(dryrun) delete: %s\n", r.RemotePath(remotePath))
		return nil
	}

	_, err := r.client.DeleteObject(context.TODO(), &s3.DeleteObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(remotePath),
	})
	if err != nil {
		return err
	}
	log.Printf("delete: %s\n", r.RemotePath(remotePath))
	return nil
}

func normalizePath(path string) string {
	return strings.ReplaceAll(path, "\\", "/")
}

func shouldExclude(fullpath string, excludePatterns []string) bool {
	for _, pattern := range excludePatterns {
		matched, err := path.Match(pattern, fullpath)
		if err == nil && matched {
			return true
		}
		// check any part of the path
		parts := strings.Split(fullpath, "/")
		for _, part := range parts {
			matched, err := path.Match(pattern, part)
			if err == nil && matched {
				return true
			}
		}
	}
	return false
}

func calcETag(path string) (etag string, err error) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	hash := md5.New()
	if _, err = io.Copy(hash, file); err != nil {
		return
	}
	etag = "\"" + hex.EncodeToString(hash.Sum(nil)) + "\""
	return etag, nil
}

func (r *R2Client) Sync(localPath, remotePath string, deleteSync bool, dryRun bool, recursive bool, concurrency int, sizeOnly bool, excludePatterns stringSliceFlag) error {
	log.Printf("Getting remote file list: %s ...\n", remotePath)
	remoteFiles, err := r.ListObjects(remotePath)
	if err != nil {
		return fmt.Errorf("failed to get remote file list: %v", err)
	}

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, concurrency)
	uploadCount := 0
	err = filepath.Walk(localPath, func(fullpath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		fullpath = normalizePath(fullpath)
		if shouldExclude(fullpath, excludePatterns) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if !recursive && path.Dir(fullpath) != localPath {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}

		relPath, _ := filepath.Rel(localPath, fullpath)
		relPath = normalizePath(relPath)
		remoteKey := path.Join(remotePath, relPath)

		needUpload := false
		if remoteInfo, exists := remoteFiles[remoteKey]; !exists {
			needUpload = true
		} else {
			if sizeOnly {
				needUpload = info.Size() != remoteInfo.Size
			} else {
				etag, err := calcETag(fullpath)
				if err != nil {
					return err
				}
				needUpload = info.Size() != remoteInfo.Size || etag != remoteInfo.ETag
			}
		}
		if needUpload {
			wg.Add(1)
			uploadCount++

			semaphore <- struct{}{}
			go func(localPath, remoteKey string) {
				defer wg.Done()
				defer func() { <-semaphore }()

				fullKey := r.RemotePath(remoteKey)
				log.Printf("uploading %s -> %s ...\n", localPath, fullKey)
				if err := r.UploadFile(localPath, remoteKey, dryRun); err != nil {
					log.Printf("upload failed %s: %v\n", fullKey, err)
				}
			}(fullpath, remoteKey)
		}

		delete(remoteFiles, remoteKey)

		return nil
	})

	if err != nil {
		return fmt.Errorf("upload failed: %v", err)
	}

	wg.Wait()
	log.Printf("%d files uploaded.\n", uploadCount)

	if deleteSync && len(remoteFiles) > 0 {
		log.Printf("Starting file deletion...\n")
		deleteCount := 0

		for remoteKey := range remoteFiles {
			wg.Add(1)
			deleteCount++
			semaphore <- struct{}{}

			go func(key string) {
				defer wg.Done()
				defer func() { <-semaphore }()
				fullKey := r.RemotePath(key)
				log.Printf("deleting %s ...\n", fullKey)
				if err := r.DeleteObject(key, dryRun); err != nil {
					log.Printf("delete failed %s: %v\n", fullKey, err)
				}
			}(remoteKey)
		}

		wg.Wait()
		log.Printf("%d files deleted.\n", deleteCount)
	}

	log.Println("Sync completed.")
	return nil
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: r2sync [--dryrun] [--delete] [--recursive] [--concurrency N] [[--exclude PATTERN] ...] [--size-only] <source path> <target path>
Options:
  --concurrency (number)
    	Number of concurrent upload/delete operations, default is 5
  --delete (boolean)
    	Delete files that exist in the target location but not in the source location
  --dryrun (boolean)
    	Only display the operations to be performed, without actually executing them
  --exclude (pattern)
    	Exclude file or directory patterns, can be used multiple times
  --recursive (boolean)
    	Recursively synchronize subdirectories
  --size-only (boolean)
    	Only use file size to determine if files are the same

Examples:
    r2sync /local/dir r2://bucket/path/
    r2sync --delete --dryrun /local/dir r2://bucket/path/
    r2sync --recursive --delete --dryrun /local/dir r2://bucket/path/
    r2sync --recursive --delete --dryrun --concurrency 10 /local/dir r2://bucket/path/
    r2sync --exclude '*.tmp' --exclude '/local/dir/exclude1' --recursive --delete --dryrun /local/dir r2://bucket/path/`)
}

func main() {
	dryRun := flag.Bool("dryrun", false, "Only display the operations to be performed, without actually executing them")
	delete := flag.Bool("delete", false, "Delete files that exist in the target location but not in the source location")
	recursive := flag.Bool("recursive", false, "Recursively synchronize subdirectories")
	concurrency := flag.Int("concurrency", 5, "Number of concurrent upload/delete operations")
	sizeOnly := flag.Bool("size-only", false, "Only use file size to determine if files are the same")
	var excludePatterns stringSliceFlag
	flag.Var(&excludePatterns, "exclude", "Exclude file or directory patterns, can be used multiple times")
	flag.Parse()

	args := flag.Args()
	if len(args) != 2 {
		usage()
		os.Exit(1)
	}

	sourcePath := normalizePath(args[0])
	u, err := url.Parse(normalizePath(args[1]))
	if err != nil {
		fmt.Println("Invalid target path: ", err)
		fmt.Println()
		usage()
		os.Exit(1)
	}
	targetPath := strings.TrimPrefix(u.Path, "/")
	bucket := u.Host
	for i, pattern := range excludePatterns {
		excludePatterns[i] = normalizePath(pattern)
	}

	client := NewR2Client(bucket, u.Scheme)
	err = client.Sync(sourcePath, targetPath, *delete, *dryRun, *recursive, *concurrency, *sizeOnly, excludePatterns)
	if err != nil {
		log.Fatal(err)
	}
}
