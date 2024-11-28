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
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type R2Client struct {
	client *s3.Client
	bucket string
}

type FileInfo struct {
	Path         string
	Size         int64
	LastModified time.Time
	ETag         string
}

func NewR2Client(bucket string) *R2Client {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		log.Fatal(err)
	}

	return &R2Client{
		client: s3.NewFromConfig(cfg),
		bucket: bucket,
	}
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

// Get local file information
func getLocalFiles(localPath string, recursive bool) (map[string]FileInfo, error) {
	result := make(map[string]FileInfo)
	err := filepath.Walk(localPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// If not in recursive mode, skip files in subdirectories
		if !recursive && filepath.Dir(path) != localPath {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if !info.IsDir() {
			relPath, _ := filepath.Rel(localPath, path)
			// Use forward slash for consistency
			relPath = strings.ReplaceAll(relPath, "\\", "/")

			// Calculate the MD5 of the file
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()

			hash := md5.New()
			if _, err := io.Copy(hash, file); err != nil {
				return err
			}
			etag := "\"" + hex.EncodeToString(hash.Sum(nil)) + "\""

			result[relPath] = FileInfo{
				Path:         relPath,
				Size:         info.Size(),
				LastModified: info.ModTime(),
				ETag:         etag,
			}
		}
		return nil
	})
	return result, err
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

func (r *R2Client) UploadFile(localPath, remotePath string, dryRun bool) error {
	if dryRun {
		log.Printf("Will upload: %s -> %s\n", localPath, remotePath)
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
		contentType = "application/octet-stream" // Default type
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
	log.Printf("Upload completed: %s -> %s, average speed: %s\n", localPath, remotePath, speedStr)

	return nil
}

func (r *R2Client) DeleteObject(remotePath string, dryRun bool) error {
	if dryRun {
		log.Printf("Will delete: %s\n", remotePath)
		return nil
	}

	_, err := r.client.DeleteObject(context.TODO(), &s3.DeleteObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(remotePath),
	})
	return err
}

func (r *R2Client) Sync(localPath, remotePath string, delete bool, dryRun bool, recursive bool, concurrency int) error {
	log.Printf("Getting local file list: %s ...\n", localPath)
	localFiles, err := getLocalFiles(localPath, recursive)
	if err != nil {
		return fmt.Errorf("failed to get local file list: %v", err)
	}

	log.Printf("Getting remote file list: %s ...\n", remotePath)
	remoteFiles, err := r.ListObjects(remotePath)
	if err != nil {
		return fmt.Errorf("failed to get remote file list: %v", err)
	}

	// Calculate files to upload and delete
	var toUpload []string
	var toDelete []string

	for localKey, localInfo := range localFiles {
		remoteKey := filepath.Join(remotePath, localKey)
		remoteKey = strings.ReplaceAll(remoteKey, "\\", "/")

		if remoteInfo, exists := remoteFiles[remoteKey]; !exists {
			toUpload = append(toUpload, localKey)
		} else if localInfo.Size != remoteInfo.Size || localInfo.ETag != remoteInfo.ETag {
			toUpload = append(toUpload, localKey)
		}
	}

	if delete {
		for remoteKey := range remoteFiles {
			localKey := strings.TrimPrefix(remoteKey, remotePath)
			localKey = strings.TrimPrefix(localKey, "/")
			if _, exists := localFiles[localKey]; !exists {
				toDelete = append(toDelete, remoteKey)
			}
		}
	}

	log.Printf("%d files need to be uploaded\n", len(toUpload))
	log.Printf("%d files need to be deleted\n", len(toDelete))

	// Execute synchronization
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, concurrency)

	// Upload files
	if len(toUpload) > 0 {
		log.Printf("Starting file upload...\n")
		for _, key := range toUpload {
			wg.Add(1)
			semaphore <- struct{}{}

			go func(key string) {
				defer wg.Done()
				defer func() { <-semaphore }()

				localFilePath := strings.ReplaceAll(filepath.Join(localPath, key), "\\", "/")
				remoteKey := filepath.Join(remotePath, key)
				remoteKey = strings.ReplaceAll(remoteKey, "\\", "/")
				fullKey := fmt.Sprintf("r2://%s/%s", r.bucket, remoteKey)
				log.Printf("Uploading %s -> %s ...\n", localFilePath, fullKey)
				if err := r.UploadFile(localFilePath, remoteKey, dryRun); err != nil {
					log.Printf("Upload failed %s: %v\n", fullKey, err)
				}
			}(key)
		}
		wg.Wait() // Wait for all uploads to complete
	}

	// Delete files
	if delete {
		if len(toDelete) > 0 {
			log.Printf("Starting file deletion...\n")
			for _, key := range toDelete {
				wg.Add(1)
				semaphore <- struct{}{}

				go func(key string) {
					defer wg.Done()
					defer func() { <-semaphore }()
					fullKey := fmt.Sprintf("r2://%s/%s", r.bucket, key)

					if err := r.DeleteObject(key, dryRun); err != nil {
						log.Printf("Delete failed %s: %v\n", fullKey, err)
					}
					log.Printf("Successfully deleted %s\n", fullKey)
				}(key)
			}
		}
		wg.Wait() // Wait for all deletions to complete
	}

	wg.Wait()
	log.Println("Sync completed")
	return nil
}

func main() {
	dryRun := flag.Bool("dryRun", false, "Only display the operations to be performed, without actually executing them")
	delete := flag.Bool("delete", false, "Delete files that exist in the target location but not in the source location")
	recursive := flag.Bool("recursive", false, "Recursively synchronize subdirectories")
	concurrency := flag.Int("concurrency", 5, "Number of concurrent upload/delete operations")
	flag.Parse()

	args := flag.Args()
	if len(args) != 2 {
		fmt.Println("Usage: r2sync [--dryRun] [--delete] [--recursive] [--concurrency N] <source path> <target path>")
		fmt.Println("Example: r2sync ./local/dir r2://bucket/path/")
		fmt.Println("         r2sync --delete --dryRun ./local/dir r2://bucket/path/")
		fmt.Println("         r2sync --recursive --delete --dryRun ./local/dir r2://bucket/path/")
		fmt.Println("         r2sync --recursive --delete --dryRun --concurrency 10 ./local/dir r2://bucket/path/")
		os.Exit(1)
	}

	sourcePath := args[0]
	targetPath := strings.TrimPrefix(strings.TrimPrefix(args[1], "r2://"), "s3://")
	paths := strings.Split(targetPath, "/")
	if len(paths) == 0 {
		fmt.Println("Target path cannot be empty")
		os.Exit(1)
	}
	bucket := paths[0]
	targetPath = strings.Join(paths[1:], "/")

	client := NewR2Client(bucket)
	err := client.Sync(sourcePath, targetPath, *delete, *dryRun, *recursive, *concurrency)
	if err != nil {
		log.Fatal(err)
	}
}
